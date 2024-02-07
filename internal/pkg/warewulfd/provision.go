package warewulfd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/container"
	"github.com/warewulf/warewulf/internal/pkg/kernel"
	"github.com/warewulf/warewulf/internal/pkg/overlay"
	"github.com/warewulf/warewulf/internal/pkg/util"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

type templateVars struct {
	Message        string
	WaitTime       string
	Hostname       string
	Fqdn           string
	Id             string
	Cluster        string
	ContainerName  string
	Hwaddr         string
	Ipaddr         string
	Port           string
	KernelArgs     string
	KernelOverride string
}

func ProvisionSend(w http.ResponseWriter, req *http.Request) {
	wwlog.Debug("Requested URL: %s", req.URL.String())
	conf := warewulfconf.Get()
	rinfo, err := parseReq(req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		wwlog.ErrorExc(err, "Bad status")
		return
	}

	wwlog.Recv("hwaddr: %s, ipaddr: %s, stage: %s", rinfo.hwaddr, req.RemoteAddr, rinfo.stage)

	if rinfo.stage == "runtime" && conf.Warewulf.Secure {
		if rinfo.remoteport >= 1024 {
			wwlog.Denied("Non-privileged port: %s", req.RemoteAddr)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	status_stages := map[string]string{
		"efiboot": "EFI",
		"ipxe":    "IPXE",
		"kernel":  "KERNEL",
		"kmods":   "KMODS_OVERLAY",
		"system":  "SYSTEM_OVERLAY",
		"runtime": "RUNTIME_OVERLAY"}

	status_stage := status_stages[rinfo.stage]
	var stage_file string

	// TODO: when module version is upgraded to go1.18, should be 'any' type
	var tmpl_data interface{}

	node, err := GetNodeOrSetDiscoverable(rinfo.hwaddr)
	if err != nil {
		wwlog.ErrorExc(err, "")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	if node.AssetKey.Defined() && node.AssetKey.Get() != rinfo.assetkey {
		w.WriteHeader(http.StatusUnauthorized)
		wwlog.Denied("Incorrect asset key for node: %s", node.Id.Get())
		updateStatus(node.Id.Get(), status_stage, "BAD_ASSET", rinfo.ipaddr)
		return
	}

	if !node.Id.Defined() {
		wwlog.Error("%s (unknown/unconfigured node)", rinfo.hwaddr)
		if rinfo.stage == "ipxe" {
			stage_file = path.Join(conf.Paths.Sysconfdir, "/warewulf/ipxe/unconfigured.ipxe")
			tmpl_data = templateVars{
				Hwaddr: rinfo.hwaddr}
		}

	} else if rinfo.stage == "ipxe" {
		stage_file = path.Join(conf.Paths.Sysconfdir, "warewulf/ipxe/"+node.Ipxe.Get()+".ipxe")
		tmpl_data = templateVars{
			Id:             node.Id.Get(),
			Cluster:        node.ClusterName.Get(),
			Fqdn:           node.Id.Get(),
			Ipaddr:         conf.Ipaddr,
			Port:           strconv.Itoa(conf.Warewulf.Port),
			Hostname:       node.Id.Get(),
			Hwaddr:         rinfo.hwaddr,
			ContainerName:  node.ContainerName.Get(),
			KernelArgs:     node.Kernel.Args.Get(),
			KernelOverride: node.Kernel.Override.Get()}
	} else if rinfo.stage == "kernel" {
		if node.Kernel.Override.Defined() {
			stage_file = kernel.KernelImage(node.Kernel.Override.Get())
		} else if node.ContainerName.Defined() {
			stage_file = container.KernelFind(node.ContainerName.Get())

			if stage_file == "" {
				wwlog.Error("No kernel found for container %s", node.ContainerName.Get())
			}
		} else {
			wwlog.Warn("No kernel version set for node %s", node.Id.Get())
		}

	} else if rinfo.stage == "kmods" {
		if node.Kernel.Override.Defined() {
			stage_file = kernel.KmodsImage(node.Kernel.Override.Get())
		} else {
			wwlog.Warn("No kernel override modules set for node %s", node.Id.Get())
		}

	} else if rinfo.stage == "container" {
		if node.ContainerName.Defined() {
			stage_file = container.ImageFile(node.ContainerName.Get())
		} else {
			wwlog.Warn("No container set for node %s", node.Id.Get())
		}

	} else if rinfo.stage == "system" || rinfo.stage == "runtime" {
		var context string
		var request_overlays []string

		if len(rinfo.overlay) > 0 {
			request_overlays = strings.Split(rinfo.overlay, ",")
		} else {
			context = rinfo.stage
		}
		stage_file, err = getOverlayFile(
			node,
			context,
			request_overlays,
			conf.Warewulf.AutobuildOverlays)

		if err != nil {
			if errors.Is(err, overlay.ErrDoesNotExist) {
				w.WriteHeader(http.StatusNotFound)
				wwlog.ErrorExc(err, "")
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			wwlog.ErrorExc(err, "")
			return
		}
	} else if rinfo.stage == "efiboot" {
		wwlog.Debug("requested method: %s", req.Method)
		containerName := node.ContainerName.Get()
		switch rinfo.efifile {
		case "shim.efi":
			stage_file = container.ShimFind(containerName)
			if stage_file == "" {
				wwlog.ErrorExc(fmt.Errorf("could't find shim.efi"), containerName)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		case "grub.efi", "grub-tpm.efi", "grubx64.efi", "grubia32.efi", "grubaa64.efi", "grubarm.efi":
			stage_file = container.GrubFind(containerName)
			if stage_file == "" {
				wwlog.ErrorExc(fmt.Errorf("could't find grub.efi"), containerName)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		case "grub.cfg":
			stage_file = path.Join(conf.Paths.Sysconfdir, "warewulf/grub/grub.cfg.ww")
			tmpl_data = templateVars{
				Id:             node.Id.Get(),
				Cluster:        node.ClusterName.Get(),
				Fqdn:           node.Id.Get(),
				Ipaddr:         conf.Ipaddr,
				Port:           strconv.Itoa(conf.Warewulf.Port),
				Hostname:       node.Id.Get(),
				Hwaddr:         rinfo.hwaddr,
				ContainerName:  node.ContainerName.Get(),
				KernelArgs:     node.Kernel.Args.Get(),
				KernelOverride: node.Kernel.Override.Get()}
			if stage_file == "" {
				wwlog.ErrorExc(fmt.Errorf("could't find grub.cfg template"), containerName)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		default:
			wwlog.ErrorExc(fmt.Errorf("could't find efiboot file: %s", rinfo.efifile), "")
		}
	} else if rinfo.stage == "shim" {
		if node.ContainerName.Defined() {
			stage_file = container.ShimFind(node.ContainerName.Get())

			if stage_file == "" {
				wwlog.Error("No kernel found for container %s", node.ContainerName.Get())
			}
		} else {
			wwlog.Warn("No container set for this %s", node.Id.Get())
		}
	} else if rinfo.stage == "grub" {
		if node.ContainerName.Defined() {
			stage_file = container.GrubFind(node.ContainerName.Get())
			if stage_file == "" {
				wwlog.Error("No grub found for container %s", node.ContainerName.Get())
			}
		} else {
			wwlog.Warn("No conainer set for node %s", node.Id.Get())
		}
	}

	wwlog.Serv("stage_file '%s'", stage_file)

	if util.IsFile(stage_file) {

		if tmpl_data != nil {
			if rinfo.compress != "" {
				wwlog.Error("Unsupported %s compressed version for file: %s",
					rinfo.compress, stage_file)
				w.WriteHeader(http.StatusNotFound)
				return
			}

			tmpl, err := template.New(stage_file).Funcs(sprig.FuncMap()).ParseFiles(stage_file)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				wwlog.ErrorExc(err, "")
				return
			}

			// template engine writes file to buffer in case rendering fails
			var buf bytes.Buffer

			err = tmpl.Execute(&buf, tmpl_data)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				wwlog.ErrorExc(err, "")
				return
			}

			w.Header().Set("Content-Type", "text")
			w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
			_, err = buf.WriteTo(w)
			if err != nil {
				wwlog.ErrorExc(err, "")
			}

			wwlog.Send("%15s: %s", node.Id.Get(), stage_file)

		} else {
			if rinfo.compress == "gz" {
				stage_file += ".gz"

				if !util.IsFile(stage_file) {
					wwlog.Error("unprepared for compressed version of file %s",
						stage_file)
					w.WriteHeader(http.StatusNotFound)
					return
				}
			} else if rinfo.compress != "" {
				wwlog.Error("unsupported %s compressed version of file %s",
					rinfo.compress, stage_file)
				w.WriteHeader(http.StatusNotFound)
			}

			err = sendFile(w, req, stage_file, node.Id.Get())
			if err != nil {
				wwlog.ErrorExc(err, "")
				return
			}
		}

		updateStatus(node.Id.Get(), status_stage, path.Base(stage_file), rinfo.ipaddr)

	} else if stage_file == "" {
		w.WriteHeader(http.StatusBadRequest)
		wwlog.Error("No resource selected")
		updateStatus(node.Id.Get(), status_stage, "BAD_REQUEST", rinfo.ipaddr)

	} else {
		w.WriteHeader(http.StatusNotFound)
		wwlog.Error("Not found: %s", stage_file)
		updateStatus(node.Id.Get(), status_stage, "NOT_FOUND", rinfo.ipaddr)
	}

}
