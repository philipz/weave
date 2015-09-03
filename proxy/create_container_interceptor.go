package proxy

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/fsouza/go-dockerclient"

	. "github.com/weaveworks/weave/common"
	"github.com/weaveworks/weave/nameserver"
	"github.com/weaveworks/weave/router"
)

const MaxDockerHostname = 64

var (
	ErrNoCommandSpecified = errors.New("No command specified")
)

type createContainerInterceptor struct{ proxy *Proxy }

// ErrNoSuchImage replaces docker.NoSuchImage, which does not contain the image
// name, which in turn breaks docker clients post 1.7.0 since they expect the
// image name to be present in errors.
type ErrNoSuchImage struct {
	Name string
}

func (err *ErrNoSuchImage) Error() string {
	return "No such image: " + err.Name
}

func (i *createContainerInterceptor) InterceptRequest(r *http.Request) error {
	container := map[string]interface{}{}
	if err := unmarshalRequestBody(r, &container); err != nil {
		return err
	}

	config, err := lookupObject(container, "Config")
	if err != nil {
		return err
	}

	hostConfig, err := lookupObject(container, "HostConfig")
	if err != nil {
		return err
	}

	networkMode, err := lookupString(hostConfig, "NetworkMode")
	if err != nil {
		return err
	}

	env, err := lookupStringArray(config, "Env")
	if err != nil {
		return err
	}

	if cidrs, err := i.proxy.weaveCIDRsFromConfig(networkMode, env); err != nil {
		Log.Infof("Leaving container alone because %s", err)
	} else {
		Log.Infof("Creating container with WEAVE_CIDR \"%s\"", strings.Join(cidrs, " "))
		if err := i.addWeaveWaitVolume(hostConfig); err != nil {
			return err
		}
		if err := i.setWeaveWaitEntrypoint(container); err != nil {
			return err
		}
		hostname, err := i.containerHostname(r, container)
		if err != nil {
			return err
		}
		if err := i.setWeaveDNS(container, hostname); err != nil {
			return err
		}

		return marshalRequestBody(r, container)
	}

	return nil
}

func (i *createContainerInterceptor) containerHostname(r *http.Request, container map[string]interface{}) (hostname string, err error) {
	hostname = r.URL.Query().Get("name")
	if i.proxy.Config.HostnameFromLabel != "" {
		hostname, err = i.hostnameFromLabel(hostname, container)
	}
	hostname = i.proxy.hostnameMatchRegexp.ReplaceAllString(hostname, i.proxy.HostnameReplacement)
	return
}

func (i *createContainerInterceptor) hostnameFromLabel(hostname string, container map[string]interface{}) (string, error) {
	labels, err := lookupObject(container, "Labels")
	if err != nil {
		return "", err
	}

	labelIface, ok := labels[i.proxy.Config.HostnameFromLabel]
	if !ok || labelIface == nil {
		return hostname, nil
	}

	label, ok := labelIface.(string)
	if !ok {
		return "", &UnmarshalWrongTypeError{i.proxy.Config.HostnameFromLabel, "string", labelIface}
	}

	return label, nil
}

func (i *createContainerInterceptor) addWeaveWaitVolume(hostConfig map[string]interface{}) error {
	configBinds, err := lookupStringArray(hostConfig, "Binds")
	if err != nil {
		return err
	}

	var binds []string
	for _, bind := range configBinds {
		s := strings.Split(bind, ":")
		if len(s) >= 2 && s[1] == "/w" {
			continue
		}
		binds = append(binds, bind)
	}
	hostConfig["Binds"] = append(binds, fmt.Sprintf("%s:/w:ro", i.proxy.weaveWaitVolume))
	return nil
}

func (i *createContainerInterceptor) setWeaveWaitEntrypoint(container map[string]interface{}) error {
	var entrypoint []string
	if e, ok := container["Entrypoint"]; ok && e != nil {
		switch e := e.(type) {
		case string:
			entrypoint = []string{e}
		case []string:
			entrypoint = e
		case []interface{}:
			for _, s := range e {
				if s, ok := s.(string); ok {
					entrypoint = append(entrypoint, s)
				} else {
					return &UnmarshalWrongTypeError{"Entrypoint", "string or array of strings", e}
				}
			}
		default:
			return &UnmarshalWrongTypeError{"Entrypoint", "string or array of strings", e}
		}
	}

	cmd, err := lookupStringArray(container, "Cmd")
	if err != nil {
		return err
	}

	if len(entrypoint) == 0 {
		containerImage, err := lookupString(container, "Image")
		if err != nil {
			return err
		}

		image, err := i.proxy.client.InspectImage(containerImage)
		if err == docker.ErrNoSuchImage {
			return &ErrNoSuchImage{containerImage}
		} else if err != nil {
			return err
		}

		if len(cmd) == 0 {
			cmd = image.Config.Cmd
			container["Cmd"] = cmd
		}

		if entrypoint == nil {
			entrypoint = image.Config.Entrypoint
			container["Entrypoint"] = entrypoint
		}
	}

	if len(entrypoint) == 0 && len(cmd) == 0 {
		return ErrNoCommandSpecified
	}

	if len(entrypoint) == 0 || entrypoint[0] != weaveWaitEntrypoint[0] {
		container["Entrypoint"] = append(weaveWaitEntrypoint, entrypoint...)
	}

	return nil
}

func (i *createContainerInterceptor) setWeaveDNS(container map[string]interface{}, name string) error {
	if i.proxy.WithoutDNS {
		return nil
	}

	dnsDomain, dnsRunning := i.getDNSDomain()
	if !(dnsRunning || i.proxy.WithDNS) {
		return nil
	}

	hostConfig, err := lookupObject(container, "HostConfig")
	if err != nil {
		return err
	}
	dns, err := lookupStringArray(hostConfig, "Dns")
	if err != nil {
		return err
	}
	hostConfig["Dns"] = append(dns, i.proxy.dockerBridgeIP)

	hostname, err := lookupString(container, "Hostname")
	if err != nil {
		return err
	}

	if hostname == "" && name != "" {
		// Strip trailing period because it's unusual to see it used on the end of a host name
		trimmedDNSDomain := strings.TrimSuffix(dnsDomain, ".")
		if len(name)+1+len(trimmedDNSDomain) > MaxDockerHostname {
			Log.Warningf("Container name [%s] too long to be used as hostname", name)
		} else {
			hostname = name
			container["Hostname"] = name
			container["Domainname"] = trimmedDNSDomain
		}
	}

	dnsSearch, err := lookupStringArray(hostConfig, "DnsSearch")
	if err != nil {
		return err
	}
	if len(dnsSearch) == 0 {
		if hostname == "" {
			hostConfig["DnsSearch"] = []string{dnsDomain}
		} else {
			hostConfig["DnsSearch"] = []string{"."}
		}
	}

	return nil
}

func (i *createContainerInterceptor) getDNSDomain() (domain string, running bool) {
	domain = nameserver.DefaultDomain
	weaveContainer, err := i.proxy.client.InspectContainer("weave")
	if err != nil ||
		weaveContainer.NetworkSettings == nil ||
		weaveContainer.NetworkSettings.IPAddress == "" {
		return
	}

	url := fmt.Sprintf("http://%s:%d/domain", weaveContainer.NetworkSettings.IPAddress, router.HTTPPort)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	return string(b), true
}

func (i *createContainerInterceptor) InterceptResponse(r *http.Response) error {
	return nil
}
