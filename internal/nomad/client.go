// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package nomad wraps the Nomad HTTP API (github.com/hashicorp/nomad/api,
// MPL-2.0) behind the interface the controller and API server use: a
// context on every call, "not found" as a nil result instead of an error,
// and an interface the tests can fake.
package nomad

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// API is what CYSTEMD asks of a Nomad cluster.
type API interface {
	// ParseHCL parses an HCL2 jobspec server-side (canonicalized).
	ParseHCL(ctx context.Context, hcl, variables string) (*api.Job, error)
	// Job returns the live job, or nil when it does not exist.
	Job(ctx context.Context, namespace, id string) (*api.Job, error)
	// ManagedJobs lists the jobs whose meta tracks app ("" = any app) in
	// namespace ("*" = all namespaces).
	ManagedJobs(ctx context.Context, namespace, app string) ([]*api.JobListStub, error)
	// Jobs lists the jobs of a namespace, with their meta.
	Jobs(ctx context.Context, namespace string) ([]*api.JobListStub, error)
	Plan(ctx context.Context, job *api.Job) (*api.JobPlanResponse, error)
	Register(ctx context.Context, job *api.Job, opts *api.RegisterOptions) (*api.JobRegisterResponse, error)
	Deregister(ctx context.Context, namespace, id string, purge bool) (string, error)
	Revert(ctx context.Context, namespace, id string, version uint64) (*api.JobRegisterResponse, error)
	TagVersion(ctx context.Context, namespace, id string, version uint64, name, description string) error
	Versions(ctx context.Context, namespace, id string) ([]*api.Job, error)
	Allocations(ctx context.Context, namespace, id string) ([]*api.AllocationListStub, error)
	LatestDeployment(ctx context.Context, namespace, id string) (*api.Deployment, error)
	Summary(ctx context.Context, namespace, id string) (*api.JobSummary, error)
	Evaluations(ctx context.Context, namespace, id string) ([]*api.Evaluation, error)
	Deployment(ctx context.Context, id string) (*api.Deployment, error)
	PromoteDeployment(ctx context.Context, id string) error
	PauseDeployment(ctx context.Context, id string, pause bool) error
	FailDeployment(ctx context.Context, id string) error
	Allocation(ctx context.Context, id string) (*api.Allocation, error)
	AllocChecks(ctx context.Context, id string) (api.AllocCheckStatuses, error)
	AllocServices(ctx context.Context, id string) ([]*api.ServiceRegistration, error)
	RestartAllocation(ctx context.Context, id, task string) error
	StopAllocation(ctx context.Context, id string) error
	SignalAllocation(ctx context.Context, id, task, signal string) error
	Logs(ctx context.Context, alloc *api.Allocation, task, logType, origin string, offset int64, follow bool) (<-chan *api.StreamFrame, <-chan error)
	Events(ctx context.Context, topics map[api.Topic][]string, index uint64) (<-chan *api.Events, error)
	EnsureNamespace(ctx context.Context, namespace string) error
	Info(ctx context.Context) (*ClusterInfo, error)
	Nodes(ctx context.Context) ([]*api.NodeListStub, error)
	NodePools(ctx context.Context) ([]*api.NodePool, error)
	Namespaces(ctx context.Context) ([]string, error)
	// Address is the API address (for UI links).
	Address() string
}

// ClusterInfo describes a cluster for the Clusters page.
type ClusterInfo struct {
	Version    string   `json:"version"`
	Region     string   `json:"region"`
	Regions    []string `json:"regions"`
	Datacenter string   `json:"datacenter"`
	Leader     string   `json:"leader"`
}

// Config connects to a Nomad cluster.
type Config struct {
	Address    string
	Region     string
	Namespace  string
	Token      string
	CACert     string
	ClientCert string
	ClientKey  string
	ServerName string
	TLSSkip    bool
}

// Client implements API over the Nomad HTTP API.
type Client struct {
	c       *api.Client
	address string
}

// New returns a client.
func New(cfg Config) (*Client, error) {
	c := api.DefaultConfig()
	if cfg.Address != "" {
		c.Address = cfg.Address
	}
	if cfg.Region != "" {
		c.Region = cfg.Region
	}
	if cfg.Token != "" {
		c.SecretID = cfg.Token
	}
	if cfg.CACert != "" || cfg.ClientCert != "" || cfg.ServerName != "" || cfg.TLSSkip {
		c.TLSConfig = &api.TLSConfig{CACert: cfg.CACert, ClientCert: cfg.ClientCert, ClientKey: cfg.ClientKey,
			TLSServerName: cfg.ServerName, Insecure: cfg.TLSSkip}
	}
	client, err := api.NewClient(c)
	if err != nil {
		return nil, err
	}
	return &Client{c: client, address: c.Address}, nil
}

// Raw exposes the underlying client.
func (c *Client) Raw() *api.Client { return c.c }

func (c *Client) Address() string { return c.address }

func q(ctx context.Context, namespace string) *api.QueryOptions {
	return (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
}

func w(ctx context.Context, namespace string) *api.WriteOptions {
	return (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)
}

// IsNotFound reports whether err is a Nomad 404.
func IsNotFound(err error) bool {
	var ue api.UnexpectedResponseError
	if errors.As(err, &ue) && ue.StatusCode() == http.StatusNotFound {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "404")
}

func (c *Client) ParseHCL(ctx context.Context, hcl, variables string) (*api.Job, error) {
	// ParseHCLOpts has no context variant; the request is small and local.
	return c.c.Jobs().ParseHCLOpts(&api.JobsParseRequest{JobHCL: hcl, Variables: variables, Canonicalize: true})
}

func (c *Client) Job(ctx context.Context, namespace, id string) (*api.Job, error) {
	job, _, err := c.c.Jobs().Info(id, q(ctx, namespace))
	if IsNotFound(err) {
		return nil, nil
	}
	return job, err
}

func (c *Client) ManagedJobs(ctx context.Context, namespace, app string) ([]*api.JobListStub, error) {
	qo := q(ctx, namespace)
	if app != "" {
		qo.Filter = fmt.Sprintf(`Meta[%q] == %q`, v1alpha1.AppMetaKey, app)
	} else {
		qo.Filter = fmt.Sprintf(`Meta[%q] is not empty`, v1alpha1.AppMetaKey)
	}
	stubs, _, err := c.c.Jobs().ListOptions(&api.JobListOptions{Fields: &api.JobListFields{Meta: true}}, qo)
	return stubs, err
}

func (c *Client) Jobs(ctx context.Context, namespace string) ([]*api.JobListStub, error) {
	stubs, _, err := c.c.Jobs().ListOptions(&api.JobListOptions{Fields: &api.JobListFields{Meta: true}}, q(ctx, namespace))
	return stubs, err
}

func (c *Client) Plan(ctx context.Context, job *api.Job) (*api.JobPlanResponse, error) {
	resp, _, err := c.c.Jobs().PlanOpts(job, &api.PlanOptions{Diff: true}, w(ctx, deref(job.Namespace)))
	return resp, err
}

func (c *Client) Register(ctx context.Context, job *api.Job, opts *api.RegisterOptions) (*api.JobRegisterResponse, error) {
	resp, _, err := c.c.Jobs().RegisterOpts(job, opts, w(ctx, deref(job.Namespace)))
	return resp, err
}

func (c *Client) Deregister(ctx context.Context, namespace, id string, purge bool) (string, error) {
	eval, _, err := c.c.Jobs().Deregister(id, purge, w(ctx, namespace))
	return eval, err
}

func (c *Client) Revert(ctx context.Context, namespace, id string, version uint64) (*api.JobRegisterResponse, error) {
	resp, _, err := c.c.Jobs().Revert(id, version, nil, w(ctx, namespace), "", "")
	return resp, err
}

func (c *Client) TagVersion(ctx context.Context, namespace, id string, version uint64, name, description string) error {
	_, err := c.c.Jobs().TagVersion(id, version, name, description, w(ctx, namespace))
	return err
}

func (c *Client) Versions(ctx context.Context, namespace, id string) ([]*api.Job, error) {
	versions, _, _, err := c.c.Jobs().Versions(id, false, q(ctx, namespace))
	return versions, err
}

// Allocations lists the job's allocations; not those of an earlier job with
// the same ID that was purged (a hook job is recreated on every run).
func (c *Client) Allocations(ctx context.Context, namespace, id string) ([]*api.AllocationListStub, error) {
	allocs, _, err := c.c.Jobs().Allocations(id, false, q(ctx, namespace))
	return allocs, err
}

func (c *Client) LatestDeployment(ctx context.Context, namespace, id string) (*api.Deployment, error) {
	d, _, err := c.c.Jobs().LatestDeployment(id, q(ctx, namespace))
	if IsNotFound(err) {
		return nil, nil
	}
	return d, err
}

func (c *Client) Summary(ctx context.Context, namespace, id string) (*api.JobSummary, error) {
	s, _, err := c.c.Jobs().Summary(id, q(ctx, namespace))
	if IsNotFound(err) {
		return nil, nil
	}
	return s, err
}

func (c *Client) Evaluations(ctx context.Context, namespace, id string) ([]*api.Evaluation, error) {
	evals, _, err := c.c.Jobs().Evaluations(id, q(ctx, namespace))
	return evals, err
}

func (c *Client) Deployment(ctx context.Context, id string) (*api.Deployment, error) {
	d, _, err := c.c.Deployments().Info(id, q(ctx, "*"))
	if IsNotFound(err) {
		return nil, nil
	}
	return d, err
}

func (c *Client) PromoteDeployment(ctx context.Context, id string) error {
	_, _, err := c.c.Deployments().PromoteAll(id, w(ctx, "*"))
	return err
}

func (c *Client) PauseDeployment(ctx context.Context, id string, pause bool) error {
	_, _, err := c.c.Deployments().Pause(id, pause, w(ctx, "*"))
	return err
}

func (c *Client) FailDeployment(ctx context.Context, id string) error {
	_, _, err := c.c.Deployments().Fail(id, w(ctx, "*"))
	return err
}

func (c *Client) Allocation(ctx context.Context, id string) (*api.Allocation, error) {
	a, _, err := c.c.Allocations().Info(id, q(ctx, "*"))
	if IsNotFound(err) {
		return nil, nil
	}
	return a, err
}

func (c *Client) AllocChecks(ctx context.Context, id string) (api.AllocCheckStatuses, error) {
	return c.c.Allocations().Checks(id, q(ctx, "*"))
}

func (c *Client) AllocServices(ctx context.Context, id string) ([]*api.ServiceRegistration, error) {
	s, _, err := c.c.Allocations().Services(id, q(ctx, "*"))
	return s, err
}

func (c *Client) RestartAllocation(ctx context.Context, id, task string) error {
	alloc, err := c.Allocation(ctx, id)
	if err != nil || alloc == nil {
		return fmt.Errorf("allocation %s: %v", id, orNotFound(err))
	}
	if task == "" {
		return c.c.Allocations().RestartAllTasks(alloc, q(ctx, alloc.Namespace))
	}
	return c.c.Allocations().Restart(alloc, task, q(ctx, alloc.Namespace))
}

func (c *Client) StopAllocation(ctx context.Context, id string) error {
	alloc, err := c.Allocation(ctx, id)
	if err != nil || alloc == nil {
		return fmt.Errorf("allocation %s: %v", id, orNotFound(err))
	}
	_, err = c.c.Allocations().Stop(alloc, q(ctx, alloc.Namespace))
	return err
}

func (c *Client) SignalAllocation(ctx context.Context, id, task, signal string) error {
	alloc, err := c.Allocation(ctx, id)
	if err != nil || alloc == nil {
		return fmt.Errorf("allocation %s: %v", id, orNotFound(err))
	}
	return c.c.Allocations().Signal(alloc, q(ctx, alloc.Namespace), task, signal)
}

func orNotFound(err error) error {
	if err == nil {
		return errors.New("not found")
	}
	return err
}

func (c *Client) Logs(ctx context.Context, alloc *api.Allocation, task, logType, origin string, offset int64, follow bool) (<-chan *api.StreamFrame, <-chan error) {
	cancel := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(cancel)
	}()
	return c.c.AllocFS().Logs(alloc, follow, task, logType, origin, offset, cancel, q(ctx, alloc.Namespace))
}

func (c *Client) Events(ctx context.Context, topics map[api.Topic][]string, index uint64) (<-chan *api.Events, error) {
	return c.c.EventStream().Stream(ctx, topics, index, q(ctx, "*"))
}

func (c *Client) EnsureNamespace(ctx context.Context, namespace string) error {
	if namespace == "" || namespace == "default" {
		return nil
	}
	if _, _, err := c.c.Namespaces().Info(namespace, q(ctx, "")); err == nil {
		return nil
	}
	_, err := c.c.Namespaces().Register(&api.Namespace{Name: namespace, Description: "Created by CYSTEMD"}, w(ctx, ""))
	return err
}

func (c *Client) Info(ctx context.Context) (*ClusterInfo, error) {
	self, err := c.c.Agent().Self()
	if err != nil {
		return nil, err
	}
	info := &ClusterInfo{}
	if self.Member.Tags != nil {
		info.Version = self.Member.Tags["build"]
		info.Region = self.Member.Tags["region"]
		info.Datacenter = self.Member.Tags["dc"]
	}
	if cfg, ok := self.Config["Version"].(map[string]any); ok {
		if v, ok := cfg["Version"].(string); ok && v != "" {
			info.Version = v
		}
	}
	if cfg, ok := self.Config["Region"].(string); ok && info.Region == "" {
		info.Region = cfg
	}
	info.Regions, _ = c.c.Regions().List()
	info.Leader, _ = c.c.Status().Leader()
	return info, nil
}

func (c *Client) Nodes(ctx context.Context) ([]*api.NodeListStub, error) {
	nodes, _, err := c.c.Nodes().List(q(ctx, ""))
	return nodes, err
}

func (c *Client) NodePools(ctx context.Context) ([]*api.NodePool, error) {
	pools, _, err := c.c.NodePools().List(q(ctx, ""))
	return pools, err
}

func (c *Client) Namespaces(ctx context.Context) ([]string, error) {
	nss, _, err := c.c.Namespaces().List(q(ctx, ""))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nss))
	for _, ns := range nss {
		out = append(out, ns.Name)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Clusters maps destination names to clients.
type Clusters struct {
	byName map[string]API
	order  []string
	labels map[string]map[string]string
}

// NewClusters returns an empty registry.
func NewClusters() *Clusters {
	return &Clusters{byName: map[string]API{}, labels: map[string]map[string]string{}}
}

// SetLabels describes a cluster (for ApplicationSets' clusters
// generators).
func (c *Clusters) SetLabels(name string, labels map[string]string) { c.labels[name] = labels }

// Labels returns a cluster's labels.
func (c *Clusters) Labels(name string) map[string]string { return c.labels[name] }

// Add registers a cluster.
func (c *Clusters) Add(name string, client API) {
	if _, ok := c.byName[name]; !ok {
		c.order = append(c.order, name)
	}
	c.byName[name] = client
}

// Get returns the cluster for a destination name ("" = in-cluster).
func (c *Clusters) Get(name string) (API, error) {
	if name == "" {
		name = v1alpha1.InClusterName
	}
	client, ok := c.byName[name]
	if !ok {
		return nil, fmt.Errorf("cluster %q is not registered", name)
	}
	return client, nil
}

// Names lists the registered clusters in registration order.
func (c *Clusters) Names() []string { return append([]string(nil), c.order...) }
