// Copyright 2016 go-dockerclient authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/docker/engine-api/types/swarm"
	"github.com/fsouza/go-dockerclient"
	"github.com/gorilla/mux"
)

type swarmServer struct {
	srv      *DockerServer
	mux      *mux.Router
	listener net.Listener
}

func newSwarmServer(srv *DockerServer, bind string) (*swarmServer, error) {
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, err
	}
	router := mux.NewRouter()
	router.Path("/internal/updatenodes").Methods("POST").HandlerFunc(srv.handlerWrapper(srv.internalUpdateNodes))
	server := &swarmServer{
		listener: listener,
		mux:      router,
		srv:      srv,
	}
	go http.Serve(listener, router)
	return server, nil
}

func (s *swarmServer) URL() string {
	if s.listener == nil {
		return ""
	}
	return "http://" + s.listener.Addr().String() + "/"
}

func (s *DockerServer) swarmInit(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm != nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	var req swarm.InitRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	node, err := s.initSwarmNode(req.ListenAddr, req.AdvertiseAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	node.ManagerStatus.Leader = true
	err = s.runNodeOperation(s.swarmServer.URL(), nodeOperation{
		Op:   "add",
		Node: node,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.swarm = &swarm.Swarm{
		JoinTokens: swarm.JoinTokens{
			Manager: s.generateID(),
			Worker:  s.generateID(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(s.nodeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *DockerServer) swarmInspect(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
	} else {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.swarm)
	}
}

func (s *DockerServer) swarmJoin(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm != nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	var req swarm.JoinRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(req.RemoteAddrs) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	node, err := s.initSwarmNode(req.ListenAddr, req.AdvertiseAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = s.runNodeOperation(fmt.Sprintf("http://%s", req.RemoteAddrs[0]), nodeOperation{
		Op:   "add",
		Node: node,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.swarm = &swarm.Swarm{
		JoinTokens: swarm.JoinTokens{
			Manager: s.generateID(),
			Worker:  s.generateID(),
		},
	}
	w.WriteHeader(http.StatusOK)
}

func (s *DockerServer) swarmLeave(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
	} else {
		s.swarmServer.listener.Close()
		s.swarm = nil
		s.nodes = nil
		s.swarmServer = nil
		s.nodeID = ""
		w.WriteHeader(http.StatusOK)
	}
}

func (s *DockerServer) serviceCreate(w http.ResponseWriter, r *http.Request) {
	var config swarm.ServiceSpec
	defer r.Body.Close()
	err := json.NewDecoder(r.Body).Decode(&config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.cMut.Lock()
	defer s.cMut.Unlock()
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if len(s.nodes) == 0 || s.swarm == nil {
		http.Error(w, "no swarm nodes available", http.StatusNotAcceptable)
		return
	}
	if config.Name == "" {
		config.Name = s.generateID()
	}
	for _, s := range s.services {
		if s.Spec.Name == config.Name {
			http.Error(w, "there's already a service with this name", http.StatusConflict)
			return
		}
	}
	service := swarm.Service{
		ID:   s.generateID(),
		Spec: config,
	}
	portBindings := map[docker.Port][]docker.PortBinding{}
	exposedPort := map[docker.Port]struct{}{}
	if config.EndpointSpec != nil {
		for _, p := range config.EndpointSpec.Ports {
			targetPort := fmt.Sprintf("%d/%s", p.TargetPort, p.Protocol)
			portBindings[docker.Port(targetPort)] = []docker.PortBinding{
				{HostIP: "0.0.0.0", HostPort: fmt.Sprintf("%d", p.PublishedPort)},
			}
			exposedPort[docker.Port(targetPort)] = struct{}{}
		}
	}
	hostConfig := docker.HostConfig{
		PortBindings: portBindings,
	}
	dockerConfig := docker.Config{
		Cmd:          config.TaskTemplate.ContainerSpec.Args,
		Env:          config.TaskTemplate.ContainerSpec.Env,
		ExposedPorts: exposedPort,
	}
	containerCount := 1
	if service.Spec.Mode.Global != nil {
		containerCount = len(s.nodes)
	} else if repl := service.Spec.Mode.Replicated; repl != nil {
		if repl.Replicas != nil {
			containerCount = int(*repl.Replicas)
		}
	}
	for i := 0; i < containerCount; i++ {
		container := docker.Container{
			ID:         s.generateID(),
			Name:       fmt.Sprintf("%s-%d", config.Name, i),
			Image:      config.TaskTemplate.ContainerSpec.Image,
			Created:    time.Now(),
			Config:     &dockerConfig,
			HostConfig: &hostConfig,
		}
		chosenNode := s.nodes[s.nodeRR]
		s.nodeRR = (s.nodeRR + 1) % len(s.nodes)
		task := swarm.Task{
			ID:        s.generateID(),
			ServiceID: service.ID,
			NodeID:    chosenNode.ID,
			Status: swarm.TaskStatus{
				State: swarm.TaskStateReady,
				ContainerStatus: swarm.ContainerStatus{
					ContainerID: container.ID,
				},
			},
			DesiredState: swarm.TaskStateReady,
			Spec:         config.TaskTemplate,
		}
		s.tasks = append(s.tasks, &task)
		s.containers = append(s.containers, &container)
		s.notify(&container)
	}
	s.services = append(s.services, &service)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(service)
}

func (s *DockerServer) nodeUpdate(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	var n *swarm.Node
	for i := range s.nodes {
		if s.nodes[i].ID == id {
			n = &s.nodes[i]
			break
		}
	}
	if n == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var spec swarm.NodeSpec
	err := json.NewDecoder(r.Body).Decode(&spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n.Spec = spec
	err = s.runNodeOperation(s.swarmServer.URL(), nodeOperation{
		Op:   "update",
		Node: *n,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *DockerServer) nodeDelete(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	err := s.runNodeOperation(s.swarmServer.URL(), nodeOperation{
		Op: "delete",
		Node: swarm.Node{
			ID: id,
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *DockerServer) nodeInspect(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	for _, n := range s.nodes {
		if n.ID == id {
			err := json.NewEncoder(w).Encode(n)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *DockerServer) nodeList(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	err := json.NewEncoder(w).Encode(s.nodes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type nodeOperation struct {
	Op   string
	Node swarm.Node
}

func (s *DockerServer) runNodeOperation(dst string, nodeOp nodeOperation) error {
	data, err := json.Marshal(nodeOp)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/internal/updatenodes", strings.TrimRight(dst, "/"))
	rsp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	if rsp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code in updatenodes: %d", rsp.StatusCode)
	}
	return json.NewDecoder(rsp.Body).Decode(&s.nodes)
}

func (s *DockerServer) internalUpdateNodes(w http.ResponseWriter, r *http.Request) {
	propagate := r.URL.Query().Get("propagate") != "0"
	if !propagate {
		s.swarmMut.Lock()
		defer s.swarmMut.Unlock()
	}
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var nodeOp nodeOperation
	err = json.Unmarshal(data, &nodeOp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if propagate {
		for _, node := range s.nodes {
			if s.nodeID == node.ID {
				continue
			}
			url := fmt.Sprintf("http://%s/internal/updatenodes?propagate=0", node.ManagerStatus.Addr)
			_, err = http.Post(url, "application/json", bytes.NewReader(data))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	switch nodeOp.Op {
	case "add":
		s.nodes = append(s.nodes, nodeOp.Node)
	case "update":
		for i, n := range s.nodes {
			if n.ID == nodeOp.Node.ID {
				s.nodes[i] = nodeOp.Node
				break
			}
		}
	case "delete":
		for i, n := range s.nodes {
			if n.ID == nodeOp.Node.ID {
				s.nodes = append(s.nodes[:i], s.nodes[i+1:]...)
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(s.nodes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
