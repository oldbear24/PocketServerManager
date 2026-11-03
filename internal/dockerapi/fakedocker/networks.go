package fakedocker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Network is a fake network. Members maps container ids to their aliases.
type Network struct {
	Name     string
	Driver   string
	Internal bool
	Labels   map[string]string
	Members  map[string][]string
}

// FindNetwork returns a copy of the network with a name, or nil.
func (e *Engine) FindNetwork(name string) *Network {
	e.mu.Lock()
	defer e.mu.Unlock()

	network := e.networks[name]
	if network == nil {
		return nil
	}

	copied := *network
	copied.Members = make(map[string][]string, len(network.Members))
	for id, aliases := range network.Members {
		copied.Members[id] = append([]string{}, aliases...)
	}

	return &copied
}

// NetworksOf returns the names of the networks a container is on.
func (e *Engine) NetworksOf(id string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	names := []string{}
	for name, network := range e.networks {
		if _, member := network.Members[id]; member {
			names = append(names, name)
		}
	}
	return names
}

func (e *Engine) createNetwork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string            `json:"Name"`
		Driver   string            `json:"Driver"`
		Internal bool              `json:"Internal"`
		Labels   map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.networks[body.Name]; exists {
		writeError(w, http.StatusConflict, fmt.Sprintf("network with name %s already exists", body.Name))
		return
	}

	driver := body.Driver
	if driver == "" {
		driver = "bridge"
	}

	e.networks[body.Name] = &Network{
		Name:     body.Name,
		Driver:   driver,
		Internal: body.Internal,
		Labels:   body.Labels,
		Members:  map[string][]string{},
	}
	e.broadcast("network", "create", "net-"+body.Name, map[string]string{"name": body.Name})

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"Id": "net-" + body.Name})
}

func (e *Engine) networkRoute(w http.ResponseWriter, r *http.Request, rest string) {
	reference, action, _ := strings.Cut(rest, "/")
	name := strings.TrimPrefix(reference, "net-")

	e.mu.Lock()
	defer e.mu.Unlock()

	network := e.networks[name]
	if network == nil {
		writeError(w, http.StatusNotFound, "network "+name+" not found")
		return
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		writeJSON(w, map[string]any{"Id": "net-" + name, "Name": name, "Driver": network.Driver, "Internal": network.Internal, "Labels": network.Labels})

	case r.Method == http.MethodDelete && action == "":
		if len(network.Members) > 0 {
			writeError(w, http.StatusForbidden, "network "+name+" has active endpoints")
			return
		}
		delete(e.networks, name)
		e.broadcast("network", "destroy", "net-"+name, map[string]string{"name": name})
		w.WriteHeader(http.StatusNoContent)

	case action == "connect" || action == "disconnect":
		var body struct {
			Container      string `json:"Container"`
			EndpointConfig struct {
				Aliases []string `json:"Aliases"`
			} `json:"EndpointConfig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		container := e.find(body.Container)
		if container == nil {
			writeError(w, http.StatusNotFound, "no such container: "+body.Container)
			return
		}

		if action == "connect" {
			if _, member := network.Members[container.ID]; member {
				writeError(w, http.StatusForbidden, "endpoint with name "+container.Name+" already exists in network "+name)
				return
			}
			network.Members[container.ID] = body.EndpointConfig.Aliases
		} else {
			delete(network.Members, container.ID)
		}

		e.broadcast("network", action, "net-"+name, map[string]string{"name": name, "container": container.ID})
		w.WriteHeader(http.StatusOK)

	default:
		writeError(w, http.StatusNotFound, "unknown network action "+action)
	}
}

// removeFromNetworks forgets a container that was removed.
func (e *Engine) removeFromNetworks(id string) {
	for _, network := range e.networks {
		delete(network.Members, id)
	}
}
