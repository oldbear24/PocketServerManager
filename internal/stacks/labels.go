package stacks

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	yaml "go.yaml.in/yaml/v4"

	"github.com/oldbear24/PocketServerManager/internal/edge"
)

// Labels of a service that put it on domains (#25). The panel turns each
// hostname into a domain of the stack's server: its DNS records, its
// certificate and its route on the edge follow from there.
const (
	// LabelDomain names the hostnames, separated by commas.
	LabelDomain = "pocketcloud.domain"
	// LabelPort is the port the service listens on inside its container; a
	// service with a single port in expose or ports needs none.
	LabelPort = "pocketcloud.port"
	// LabelTLS is acme (the default) or off.
	LabelTLS = "pocketcloud.tls"
	// LabelAuth asks for a password: user:bcrypt-hash, separated by commas.
	LabelAuth = "pocketcloud.auth"
)

// TLS of a label domain.
const (
	TLSACME     = "acme"
	TLSOff      = "off"
	TLSInternal = "internal"
)

// Exposure is a service that its labels put on domains.
type Exposure struct {
	Service string `json:"service"`
	// Container is the name Docker gives the service's first container.
	Container string               `json:"container"`
	Hostnames []string             `json:"hostnames"`
	Port      int                  `json:"port"`
	TLS       string               `json:"tls"`
	BasicAuth []edge.BasicAuthUser `json:"-"`
}

// Exposures reads the pocketcloud labels of a project's services. A service
// whose labels are wrong is left out, and the problems say why. checkHashes
// checks the password hashes too, which an editor without the stack's
// secrets cannot do when they come from a secret.
func Exposures(project *types.Project, tree *yaml.Node, checkHashes bool) ([]Exposure, []Problem) {
	exposures := []Exposure{}
	problems := []Problem{}
	claimed := map[string]string{}

	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		service := project.Services[name]
		labels := service.Labels
		at := func(label, message string) Problem {
			path := []string{"services", name, "labels"}
			if label != "" {
				path = append(path, label)
			}
			line, column := lineOf(tree, path)
			return Problem{File: FileCompose, Line: line, Column: column, Message: name + ": " + message}
		}

		domains, hasDomain := labels[LabelDomain]
		if !hasDomain {
			for _, label := range []string{LabelPort, LabelTLS, LabelAuth} {
				if _, ok := labels[label]; ok {
					problems = append(problems, at(label, fmt.Sprintf("%s does nothing without %s.", label, LabelDomain)))
				}
			}
			continue
		}

		exposure := Exposure{Service: name, Container: containerName(project.Name, name, service)}
		valid := true

		for _, hostname := range strings.FieldsFunc(domains, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
			if err := edge.ValidHostname(hostname); err != nil {
				problems = append(problems, at(LabelDomain, err.Error()+"."))
				valid = false
				continue
			}
			if other, ok := claimed[hostname]; ok {
				problems = append(problems, at(LabelDomain, fmt.Sprintf("%s is the domain of %s already.", hostname, other)))
				valid = false
				continue
			}
			claimed[hostname] = name
			if !slices.Contains(exposure.Hostnames, hostname) {
				exposure.Hostnames = append(exposure.Hostnames, hostname)
			}
		}
		if len(exposure.Hostnames) == 0 && valid {
			problems = append(problems, at(LabelDomain, LabelDomain+" names no hostname."))
			valid = false
		}

		if text, ok := labels[LabelPort]; ok {
			port, err := strconv.Atoi(strings.TrimSpace(text))
			if err != nil || port < 1 || port > 65535 {
				problems = append(problems, at(LabelPort, fmt.Sprintf("%s is a port between 1 and 65535, not %q.", LabelPort, text)))
				valid = false
			}
			exposure.Port = port
		} else {
			ports := servicePorts(service)
			switch len(ports) {
			case 1:
				exposure.Port = ports[0]
			case 0:
				problems = append(problems, at(LabelDomain, fmt.Sprintf("set %s: the service names no port in expose or ports.", LabelPort)))
				valid = false
			default:
				problems = append(problems, at(LabelDomain, fmt.Sprintf("set %s: the service listens on %s.", LabelPort, joinPorts(ports))))
				valid = false
			}
		}

		switch tls := strings.ToLower(strings.TrimSpace(labels[LabelTLS])); tls {
		case "", TLSACME:
			exposure.TLS = TLSACME
		case TLSOff:
			exposure.TLS = TLSOff
		case TLSInternal:
			problems = append(problems, at(LabelTLS, "certificates from PocketCloud's internal CA are not available yet: use acme, or off."))
			valid = false
		default:
			problems = append(problems, at(LabelTLS, fmt.Sprintf("%s is acme or off, not %q.", LabelTLS, labels[LabelTLS])))
			valid = false
		}

		if text, ok := labels[LabelAuth]; ok {
			users, err := parseAuth(text)
			if err == nil && checkHashes {
				err = edge.ValidBasicAuth(users)
			}
			if err != nil {
				problems = append(problems, at(LabelAuth, err.Error()+"."))
				valid = false
			}
			exposure.BasicAuth = users
			if exposure.TLS == TLSOff {
				problems = append(problems, at(LabelAuth, "a password needs HTTPS: remove pocketcloud.tls=off."))
				valid = false
			}
		}

		if scale := service.GetScale(); scale > 1 && valid {
			problems = append(problems, at("", fmt.Sprintf("only the first of its %d containers gets the requests of %s.", scale, strings.Join(exposure.Hostnames, ", "))))
		}

		if valid {
			exposures = append(exposures, exposure)
		}
	}
	return exposures, problems
}

// containerName is what Compose calls a service's first container.
func containerName(project, service string, config types.ServiceConfig) string {
	if config.ContainerName != "" {
		return config.ContainerName
	}
	return project + "-" + service + "-1"
}

// servicePorts are the ports a service names, inside its container.
func servicePorts(service types.ServiceConfig) []int {
	ports := []int{}
	add := func(port int) {
		if port > 0 && !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	for _, expose := range service.Expose {
		if strings.HasSuffix(expose, "/udp") {
			continue
		}
		// a single port, or a range: both ends
		first, last, _ := strings.Cut(strings.TrimSuffix(expose, "/tcp"), "-")
		for _, end := range []string{first, last} {
			if port, err := strconv.Atoi(strings.TrimSpace(end)); err == nil {
				add(port)
			}
		}
	}
	for _, port := range service.Ports {
		if port.Protocol == "" || port.Protocol == "tcp" {
			add(int(port.Target))
		}
	}
	slices.Sort(ports)
	return ports
}

func joinPorts(ports []int) string {
	texts := make([]string, len(ports))
	for i, port := range ports {
		texts[i] = strconv.Itoa(port)
	}
	if len(texts) > 1 {
		return strings.Join(texts[:len(texts)-1], ", ") + " and " + texts[len(texts)-1]
	}
	return strings.Join(texts, "")
}

// parseAuth reads user:hash pairs, separated by commas.
func parseAuth(text string) ([]edge.BasicAuthUser, error) {
	users := []edge.BasicAuthUser{}
	for _, pair := range strings.Split(text, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, hash, ok := strings.Cut(pair, ":")
		if !ok || name == "" || hash == "" {
			return nil, fmt.Errorf("%s is user:hash, separated by commas", LabelAuth)
		}
		users = append(users, edge.BasicAuthUser{Username: name, Hash: hash})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("%s names no user", LabelAuth)
	}
	return users, nil
}
