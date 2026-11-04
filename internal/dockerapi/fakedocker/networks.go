package fakedocker

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"strings"
)

// Network is a fake network. Members maps container ids to their aliases, and
// Addresses to the address each got on the network.
type Network struct {
	Name      string
	Driver    string
	Internal  bool
	Labels    map[string]string
	Members   map[string][]string
	Addresses map[string]string

	// number tells the networks' address ranges apart
	number int
}

// join adds a container under aliases and gives it an address on the network.
func (n *Network) join(id string, aliases []string) {
	if aliases == nil {
		aliases = []string{}
	}
	if n.Addresses == nil {
		n.Addresses = map[string]string{}
	}
	n.Members[id] = aliases
	n.Addresses[id] = fmt.Sprintf("10.%d.0.%d", 100+n.number%150, len(n.Members)+1)
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
	copied.Addresses = maps.Clone(network.Addresses)

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

	e.networkCount++
	e.networks[body.Name] = &Network{
		Name:      body.Name,
		Driver:    driver,
		Internal:  body.Internal,
		Labels:    body.Labels,
		Members:   map[string][]string{},
		Addresses: map[string]string{},
		number:    e.networkCount,
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
		members := map[string]any{}
		for id := range network.Members {
			if container := e.containers[id]; container != nil {
				members[id] = map[string]any{"Name": container.Name}
			}
		}
		writeJSON(w, map[string]any{"Id": "net-" + name, "Name": name, "Driver": network.Driver, "Internal": network.Internal, "Labels": network.Labels, "Containers": members})

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
			network.join(container.ID, body.EndpointConfig.Aliases)
		} else {
			delete(network.Members, container.ID)
			delete(network.Addresses, container.ID)
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
		delete(network.Addresses, id)
	}
}

// receiveArchive unpacks what was copied into a container, for tests to read with File.
func (e *Engine) receiveArchive(w http.ResponseWriter, r *http.Request, container *Container) {
	dir := r.URL.Query().Get("path")

	reader := tar.NewReader(r.Body)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if container.files == nil {
			container.files = map[string][]byte{}
		}
		container.files[path.Join(dir, header.Name)] = content //nolint:gosec // a key of an in-memory map, not a path on disk
	}

	w.WriteHeader(http.StatusOK)
}

// File returns a file that was copied into a container, or nil.
func (e *Engine) File(name, file string) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, container := range e.containers {
		if container.Name == name {
			return container.files[file]
		}
	}
	return nil
}
