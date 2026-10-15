// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 holds the CYSTEMD API objects. They follow the shape of
// Argo CD's Application, AppProject and Repository resources (same field
// names, same status vocabulary) so the Argo CD mental model — and much of
// its tooling — carries over, with Nomad jobs and Consul keys as the managed
// resources instead of Kubernetes objects.
package v1alpha1

import "time"

const (
	Group      = "cystemd.io"
	Version    = "v1alpha1"
	APIVersion = Group + "/" + Version

	KindApplication = "Application"
	KindAppProject  = "AppProject"

	// DefaultProject exists implicitly and allows every source and
	// destination.
	DefaultProject = "default"

	// InClusterName is the destination name of the Nomad cluster the
	// control plane runs in (or is configured with first).
	InClusterName = "in-cluster"

	// ResourcesFinalizer makes deleting an Application delete its jobs and
	// keys too (cascade), as in Argo CD.
	ResourcesFinalizer = "resources-finalizer.cystemd.io"

	// AppMetaKey is the Nomad job meta key tracking the owning Application
	// (Argo CD's app.kubernetes.io/instance label).
	AppMetaKey = "cystemd.app"
)

// ObjectMeta is the subset of Kubernetes object metadata CYSTEMD uses.
type ObjectMeta struct {
	Name              string            `json:"name" yaml:"name"`
	Labels            map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	CreationTimestamp *time.Time        `json:"creationTimestamp,omitempty" yaml:"creationTimestamp,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty" yaml:"deletionTimestamp,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty" yaml:"finalizers,omitempty"`
	// ResourceVersion is the store's modify index; writes with a stale value
	// are rejected (optimistic concurrency).
	ResourceVersion string `json:"resourceVersion,omitempty" yaml:"resourceVersion,omitempty"`
	// Generation increments on every spec change.
	Generation int64 `json:"generation,omitempty" yaml:"generation,omitempty"`
}

// HasFinalizer reports whether name is set.
func (m *ObjectMeta) HasFinalizer(name string) bool {
	for _, f := range m.Finalizers {
		if f == name {
			return true
		}
	}
	return false
}

// RemoveFinalizer drops name.
func (m *ObjectMeta) RemoveFinalizer(name string) {
	out := m.Finalizers[:0]
	for _, f := range m.Finalizers {
		if f != name {
			out = append(out, f)
		}
	}
	m.Finalizers = out
}

// Application is a group of Nomad jobs and Consul keys rendered from a Git
// source and kept in sync with it.
type Application struct {
	APIVersion string            `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty" yaml:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata" yaml:"metadata"`
	Spec       ApplicationSpec   `json:"spec" yaml:"spec"`
	Status     ApplicationStatus `json:"status,omitempty" yaml:"status,omitempty"`
	Operation  *Operation        `json:"operation,omitempty" yaml:"operation,omitempty"`
}

// ApplicationSpec is the desired state of an Application.
type ApplicationSpec struct {
	Project              string                      `json:"project" yaml:"project"`
	Source               ApplicationSource           `json:"source" yaml:"source"`
	Destination          ApplicationDestination      `json:"destination" yaml:"destination"`
	SyncPolicy           *SyncPolicy                 `json:"syncPolicy,omitempty" yaml:"syncPolicy,omitempty"`
	IgnoreDifferences    []ResourceIgnoreDifferences `json:"ignoreDifferences,omitempty" yaml:"ignoreDifferences,omitempty"`
	RevisionHistoryLimit *int64                      `json:"revisionHistoryLimit,omitempty" yaml:"revisionHistoryLimit,omitempty"`
	Info                 []Info                      `json:"info,omitempty" yaml:"info,omitempty"`
}

// DefaultRevisionHistoryLimit is the number of history entries kept.
const DefaultRevisionHistoryLimit = 10

// HistoryLimit returns the configured history limit or the default.
func (s *ApplicationSpec) HistoryLimit() int {
	if s.RevisionHistoryLimit != nil && *s.RevisionHistoryLimit > 0 {
		return int(*s.RevisionHistoryLimit)
	}
	return DefaultRevisionHistoryLimit
}

// ApplicationSource is where the desired state comes from.
type ApplicationSource struct {
	RepoURL string `json:"repoURL" yaml:"repoURL"`
	// Path is the directory inside the repository.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// TargetRevision is a branch, tag or commit; empty means HEAD.
	TargetRevision string `json:"targetRevision,omitempty" yaml:"targetRevision,omitempty"`

	// Exactly one renderer applies; Nomad is the default.
	Nomad   *NomadSource   `json:"nomad,omitempty" yaml:"nomad,omitempty"`
	Systemd *SystemdSource `json:"systemd,omitempty" yaml:"systemd,omitempty"`
}

// SourceType names the renderer of a source.
type SourceType string

const (
	SourceTypeNomad   SourceType = "Nomad"
	SourceTypeSystemd SourceType = "Systemd"
)

// Type returns the source's renderer.
func (s *ApplicationSource) Type() SourceType {
	if s.Systemd != nil {
		return SourceTypeSystemd
	}
	return SourceTypeNomad
}

// Revision returns TargetRevision, defaulting to HEAD.
func (s *ApplicationSource) Revision() string {
	if s.TargetRevision == "" {
		return "HEAD"
	}
	return s.TargetRevision
}

// NomadSource renders Nomad jobspecs (HCL2 or JSON) found under Path.
type NomadSource struct {
	// Files are glob patterns relative to Path; default *.nomad.hcl,
	// *.nomad and *.nomad.json.
	Files []string `json:"files,omitempty" yaml:"files,omitempty"`
	// VarFiles are HCL2 variable files relative to Path, later files
	// overriding earlier ones.
	VarFiles []string `json:"varFiles,omitempty" yaml:"varFiles,omitempty"`
	// Vars override the variable files (as `-var` would).
	Vars map[string]string `json:"vars,omitempty" yaml:"vars,omitempty"`
	// ConsulKV is a file relative to Path listing Consul keys the
	// Application manages (see docs/APPLICATIONS.md).
	ConsulKV string `json:"consulKV,omitempty" yaml:"consulKV,omitempty"`
}

// SystemdSource renders a systemd-driver job from a Helm-style values file:
// the CYSTEMD v4 continuous_deployment model, kept for existing fleets.
type SystemdSource struct {
	// ValuesFile is relative to Path (default values.yaml).
	ValuesFile string `json:"valuesFile,omitempty" yaml:"valuesFile,omitempty"`
	// Selector is substituted for ${selector} in every key below.
	Selector string `json:"selector,omitempty" yaml:"selector,omitempty"`
	// ConfigKey is a yq-style path of the service configuration inside the
	// values file; rendered to ServiceConfig. Empty: the whole file.
	ConfigKey string `json:"configKey,omitempty" yaml:"configKey,omitempty"`
	// VersionKey, CPUKey and MemoryKey are scalar paths: the package version
	// and Kubernetes-style CPU and memory limits.
	VersionKey string `json:"versionKey,omitempty" yaml:"versionKey,omitempty"`
	CPUKey     string `json:"cpuKey,omitempty" yaml:"cpuKey,omitempty"`
	MemoryKey  string `json:"memoryKey,omitempty" yaml:"memoryKey,omitempty"`

	// Unit is the systemd unit (default: the application name).
	Unit string `json:"unit,omitempty" yaml:"unit,omitempty"`
	// Package is the package name (default: unit without .service).
	Package string `json:"package,omitempty" yaml:"package,omitempty"`
	// ServiceConfig is the configuration file name (default
	// target.config.yml), installed into Workdir/<unit>/.
	ServiceConfig string `json:"serviceConfig,omitempty" yaml:"serviceConfig,omitempty"`
	// Workdir is the base directory on the host (default /opt).
	Workdir string `json:"workdir,omitempty" yaml:"workdir,omitempty"`

	// Replicas is the instance count (default 1), one per node at most. 0
	// runs one instance on every eligible node (Nomad system job).
	Replicas *int `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	// NodePool is the Nomad node pool (default "default").
	NodePool string `json:"nodePool,omitempty" yaml:"nodePool,omitempty"`
	// NodeSelector restricts placement to nodes whose client meta
	// cystemd_selector_<name> is "true" (the v4 node_pools model, where a
	// host may belong to several pools).
	NodeSelector string   `json:"nodeSelector,omitempty" yaml:"nodeSelector,omitempty"`
	Datacenters  []string `json:"datacenters,omitempty" yaml:"datacenters,omitempty"`

	// Secrets are Vault KV paths rendered into env files.
	Secrets []SystemdSecret `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	// SecretsDelivery is "nomad" (Nomad's native Vault integration and
	// template runner, default) or "vault-agent" (a Vault Agent sidecar
	// task per allocation).
	SecretsDelivery string `json:"secretsDelivery,omitempty" yaml:"secretsDelivery,omitempty"`
	// VaultRole is the Vault JWT auth role for the workload identity.
	VaultRole string `json:"vaultRole,omitempty" yaml:"vaultRole,omitempty"`

	// ConfigDelivery is "consul" (the rendered config is a managed Consul
	// key read by a template: changes restart the unit in place, no new
	// allocation) or "inline" (embedded in the job). Default: consul when
	// the control plane has Consul, else inline.
	ConfigDelivery string `json:"configDelivery,omitempty" yaml:"configDelivery,omitempty"`
	// ChangeMode is what a config or secret change does: restart (default),
	// signal or noop. ChangeSignal goes with signal.
	ChangeMode   string `json:"changeMode,omitempty" yaml:"changeMode,omitempty"`
	ChangeSignal string `json:"changeSignal,omitempty" yaml:"changeSignal,omitempty"`

	// Service registers the unit in Consul.
	Service *ServiceSpec `json:"service,omitempty" yaml:"service,omitempty"`
	// Resources is the Nomad scheduling reservation (MHz, MB); the
	// enforced limits come from CPUKey/MemoryKey.
	Resources *ResourceReservation `json:"resources,omitempty" yaml:"resources,omitempty"`
}

// SystemdSecret is one Vault KV secret rendered into an env file.
type SystemdSecret struct {
	// VaultPath is the logical KV path ("<mount>/<path>", never with
	// /data/), e.g. kubernetes/myproject/myapp.
	VaultPath string `json:"vaultPath" yaml:"vaultPath"`
	// KVVersion is 1 or 2 (default 2).
	KVVersion int `json:"kvVersion,omitempty" yaml:"kvVersion,omitempty"`
	// Destination is the env file name, installed into Workdir/<unit>/
	// (default .env).
	Destination string `json:"destination,omitempty" yaml:"destination,omitempty"`
}

// ServiceSpec is a Consul service registration with one check.
type ServiceSpec struct {
	Name  string     `json:"name,omitempty" yaml:"name,omitempty"`
	Port  int        `json:"port" yaml:"port"`
	Tags  []string   `json:"tags,omitempty" yaml:"tags,omitempty"`
	Check *CheckSpec `json:"check,omitempty" yaml:"check,omitempty"`
}

// CheckSpec is a Consul health check: http (Path) or tcp.
type CheckSpec struct {
	Type     string `json:"type,omitempty" yaml:"type,omitempty"`
	Path     string `json:"path,omitempty" yaml:"path,omitempty"`
	Interval string `json:"interval,omitempty" yaml:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// ResourceReservation is a Nomad resources block.
type ResourceReservation struct {
	CPU      int `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	MemoryMB int `json:"memoryMB,omitempty" yaml:"memoryMB,omitempty"`
}

// ApplicationDestination is the Nomad cluster, region and namespace the
// Application deploys to.
type ApplicationDestination struct {
	// Name is a cluster registered in the control plane (default
	// in-cluster).
	Name      string `json:"name,omitempty" yaml:"name,omitempty"`
	Region    string `json:"region,omitempty" yaml:"region,omitempty"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// ClusterName returns Name, defaulting to in-cluster.
func (d ApplicationDestination) ClusterName() string {
	if d.Name == "" {
		return InClusterName
	}
	return d.Name
}

// NamespaceOrDefault returns Namespace, defaulting to "default".
func (d ApplicationDestination) NamespaceOrDefault() string {
	if d.Namespace == "" {
		return "default"
	}
	return d.Namespace
}

// SyncPolicy controls when and how a sync is performed.
type SyncPolicy struct {
	Automated   *SyncPolicyAutomated `json:"automated,omitempty" yaml:"automated,omitempty"`
	SyncOptions []string             `json:"syncOptions,omitempty" yaml:"syncOptions,omitempty"`
	Retry       *RetryStrategy       `json:"retry,omitempty" yaml:"retry,omitempty"`
}

// SyncPolicyAutomated syncs automatically when the desired state changes.
type SyncPolicyAutomated struct {
	// Prune deletes jobs and keys no longer in Git.
	Prune bool `json:"prune,omitempty" yaml:"prune,omitempty"`
	// SelfHeal re-syncs when the live state drifts from Git.
	SelfHeal bool `json:"selfHeal,omitempty" yaml:"selfHeal,omitempty"`
	// AllowEmpty allows a sync that would prune every resource.
	AllowEmpty bool `json:"allowEmpty,omitempty" yaml:"allowEmpty,omitempty"`
}

// IsAutomated reports whether automated sync is on.
func (p *SyncPolicy) IsAutomated() bool { return p != nil && p.Automated != nil }

// HasOption reports whether opt ("PruneLast=true") is set.
func (p *SyncPolicy) HasOption(opt string) bool {
	if p == nil {
		return false
	}
	for _, o := range p.SyncOptions {
		if o == opt {
			return true
		}
	}
	return false
}

// Sync options understood by the controller.
const (
	SyncOptionPruneLast          = "PruneLast=true"
	SyncOptionCreateNamespace    = "CreateNamespace=true"
	SyncOptionPreserveCounts     = "PreserveCounts=true"
	SyncOptionApplyOutOfSyncOnly = "ApplyOutOfSyncOnly=true"
	SyncOptionPruneFalse         = "Prune=false"
)

// RetryStrategy retries failed syncs.
type RetryStrategy struct {
	Limit   int64    `json:"limit,omitempty" yaml:"limit,omitempty"`
	Backoff *Backoff `json:"backoff,omitempty" yaml:"backoff,omitempty"`
}

// Backoff is an exponential retry backoff.
type Backoff struct {
	Duration    string `json:"duration,omitempty" yaml:"duration,omitempty"`
	Factor      *int64 `json:"factor,omitempty" yaml:"factor,omitempty"`
	MaxDuration string `json:"maxDuration,omitempty" yaml:"maxDuration,omitempty"`
}

// ResourceIgnoreDifferences ignores fields when comparing a resource. Fields
// are dotted paths into the job JSON with * matching any key or index
// ("TaskGroups.*.Count").
type ResourceIgnoreDifferences struct {
	Kind       string   `json:"kind" yaml:"kind"`
	Name       string   `json:"name,omitempty" yaml:"name,omitempty"`
	Namespace  string   `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	JSONFields []string `json:"jsonPointers,omitempty" yaml:"jsonPointers,omitempty"`
}

// Info is a free-form name/value shown in the UI.
type Info struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value" yaml:"value"`
}

// SyncStatusCode is Synced, OutOfSync or Unknown.
type SyncStatusCode string

const (
	SyncStatusUnknown   SyncStatusCode = "Unknown"
	SyncStatusSynced    SyncStatusCode = "Synced"
	SyncStatusOutOfSync SyncStatusCode = "OutOfSync"
)

// HealthStatusCode follows Argo CD's health vocabulary.
type HealthStatusCode string

const (
	HealthStatusUnknown     HealthStatusCode = "Unknown"
	HealthStatusProgressing HealthStatusCode = "Progressing"
	HealthStatusHealthy     HealthStatusCode = "Healthy"
	HealthStatusSuspended   HealthStatusCode = "Suspended"
	HealthStatusDegraded    HealthStatusCode = "Degraded"
	HealthStatusMissing     HealthStatusCode = "Missing"
)

// healthOrder ranks health from best to worst, as Argo CD does.
var healthOrder = map[HealthStatusCode]int{
	HealthStatusHealthy:     0,
	HealthStatusSuspended:   1,
	HealthStatusProgressing: 2,
	HealthStatusMissing:     3,
	HealthStatusDegraded:    4,
	HealthStatusUnknown:     5,
}

// IsWorse reports whether candidate is worse than current.
func IsWorse(current, candidate HealthStatusCode) bool {
	return healthOrder[candidate] > healthOrder[current]
}

// HealthStatus is a health code and a human message.
type HealthStatus struct {
	Status  HealthStatusCode `json:"status,omitempty" yaml:"status,omitempty"`
	Message string           `json:"message,omitempty" yaml:"message,omitempty"`
}

// ApplicationStatus is the observed state of an Application.
type ApplicationStatus struct {
	Resources      []ResourceStatus       `json:"resources,omitempty" yaml:"resources,omitempty"`
	Sync           SyncStatus             `json:"sync,omitempty" yaml:"sync,omitempty"`
	Health         HealthStatus           `json:"health,omitempty" yaml:"health,omitempty"`
	History        []RevisionHistory      `json:"history,omitempty" yaml:"history,omitempty"`
	Conditions     []ApplicationCondition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	ReconciledAt   *time.Time             `json:"reconciledAt,omitempty" yaml:"reconciledAt,omitempty"`
	ObservedAt     *time.Time             `json:"observedAt,omitempty" yaml:"observedAt,omitempty"`
	OperationState *OperationState        `json:"operationState,omitempty" yaml:"operationState,omitempty"`
	SourceType     SourceType             `json:"sourceType,omitempty" yaml:"sourceType,omitempty"`
	Summary        ApplicationSummary     `json:"summary,omitempty" yaml:"summary,omitempty"`
}

// SyncStatus is the comparison of the live state with the desired state.
type SyncStatus struct {
	Status     SyncStatusCode `json:"status" yaml:"status"`
	ComparedTo ComparedTo     `json:"comparedTo,omitempty" yaml:"comparedTo,omitempty"`
	// Revision is the resolved commit the comparison used.
	Revision string `json:"revision,omitempty" yaml:"revision,omitempty"`
}

// ComparedTo records what the comparison was made against.
type ComparedTo struct {
	Source      ApplicationSource      `json:"source" yaml:"source"`
	Destination ApplicationDestination `json:"destination" yaml:"destination"`
}

// ResourceStatus is one managed resource (a Nomad job or Consul key).
type ResourceStatus struct {
	Kind            string         `json:"kind" yaml:"kind"`
	Namespace       string         `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name            string         `json:"name" yaml:"name"`
	Status          SyncStatusCode `json:"status,omitempty" yaml:"status,omitempty"`
	Health          *HealthStatus  `json:"health,omitempty" yaml:"health,omitempty"`
	RequiresPruning bool           `json:"requiresPruning,omitempty" yaml:"requiresPruning,omitempty"`
	// Version is the live Nomad job version.
	Version  *uint64 `json:"version,omitempty" yaml:"version,omitempty"`
	SyncWave int64   `json:"syncWave,omitempty" yaml:"syncWave,omitempty"`
}

// Kinds of managed resources.
const (
	KindJob      = "Job"
	KindConsulKV = "ConsulKV"
)

// Kinds of live (child) resources in the resource tree.
const (
	KindTaskGroup  = "TaskGroup"
	KindAllocation = "Allocation"
	KindTask       = "Task"
	KindDeployment = "Deployment"
	KindService    = "Service"
	KindNode       = "Node"
)

// RevisionHistory is one successful sync.
type RevisionHistory struct {
	ID              int64              `json:"id" yaml:"id"`
	Revision        string             `json:"revision" yaml:"revision"`
	DeployedAt      time.Time          `json:"deployedAt" yaml:"deployedAt"`
	DeployStartedAt *time.Time         `json:"deployStartedAt,omitempty" yaml:"deployStartedAt,omitempty"`
	Source          ApplicationSource  `json:"source" yaml:"source"`
	InitiatedBy     OperationInitiator `json:"initiatedBy,omitempty" yaml:"initiatedBy,omitempty"`
	// JobVersions maps "<namespace>/<job>" to the Nomad job version this
	// sync produced: rollback reverts to exactly these versions.
	JobVersions map[string]uint64 `json:"jobVersions,omitempty" yaml:"jobVersions,omitempty"`
}

// ApplicationCondition is an error or warning about the Application.
type ApplicationCondition struct {
	Type               string     `json:"type" yaml:"type"`
	Message            string     `json:"message" yaml:"message"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty" yaml:"lastTransitionTime,omitempty"`
}

// Condition types.
const (
	ConditionComparisonError = "ComparisonError"
	ConditionSyncError       = "SyncError"
	ConditionInvalidSpec     = "InvalidSpecError"
	ConditionOrphanedWarning = "OrphanedResourceWarning"
	ConditionDeletionError   = "DeletionError"
)

// ApplicationSummary carries UI hints.
type ApplicationSummary struct {
	ExternalURLs []string `json:"externalURLs,omitempty" yaml:"externalURLs,omitempty"`
	// Images lists what the jobs run: container images, packages
	// ("pkg:name@version") or commands.
	Images []string `json:"images,omitempty" yaml:"images,omitempty"`
}

// Operation is a requested action the controller executes.
type Operation struct {
	Sync        *SyncOperation     `json:"sync,omitempty" yaml:"sync,omitempty"`
	InitiatedBy OperationInitiator `json:"initiatedBy,omitempty" yaml:"initiatedBy,omitempty"`
	Info        []Info             `json:"info,omitempty" yaml:"info,omitempty"`
	Retry       *RetryStrategy     `json:"retry,omitempty" yaml:"retry,omitempty"`
}

// SyncOperation is a sync (or a rollback, when RollbackID is set).
type SyncOperation struct {
	Revision    string                  `json:"revision,omitempty" yaml:"revision,omitempty"`
	Prune       bool                    `json:"prune,omitempty" yaml:"prune,omitempty"`
	DryRun      bool                    `json:"dryRun,omitempty" yaml:"dryRun,omitempty"`
	Resources   []SyncOperationResource `json:"resources,omitempty" yaml:"resources,omitempty"`
	SyncOptions []string                `json:"syncOptions,omitempty" yaml:"syncOptions,omitempty"`
	Source      *ApplicationSource      `json:"source,omitempty" yaml:"source,omitempty"`
	// RollbackID reverts to the job versions of that history entry.
	RollbackID *int64 `json:"rollbackId,omitempty" yaml:"rollbackId,omitempty"`
}

// HasOption reports whether opt is set on the operation.
func (s *SyncOperation) HasOption(opt string) bool {
	for _, o := range s.SyncOptions {
		if o == opt {
			return true
		}
	}
	return false
}

// SyncOperationResource selects one resource for a partial sync.
type SyncOperationResource struct {
	Kind      string `json:"kind" yaml:"kind"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name      string `json:"name" yaml:"name"`
}

// OperationInitiator is who requested an operation.
type OperationInitiator struct {
	Username  string `json:"username,omitempty" yaml:"username,omitempty"`
	Automated bool   `json:"automated,omitempty" yaml:"automated,omitempty"`
}

// OperationPhase is the lifecycle phase of an operation.
type OperationPhase string

const (
	OperationRunning     OperationPhase = "Running"
	OperationTerminating OperationPhase = "Terminating"
	OperationFailed      OperationPhase = "Failed"
	OperationError       OperationPhase = "Error"
	OperationSucceeded   OperationPhase = "Succeeded"
)

// Completed reports whether the phase is final.
func (p OperationPhase) Completed() bool {
	return p == OperationFailed || p == OperationError || p == OperationSucceeded
}

// OperationState is the state of the current or last operation.
type OperationState struct {
	Operation  Operation            `json:"operation" yaml:"operation"`
	Phase      OperationPhase       `json:"phase" yaml:"phase"`
	Message    string               `json:"message,omitempty" yaml:"message,omitempty"`
	SyncResult *SyncOperationResult `json:"syncResult,omitempty" yaml:"syncResult,omitempty"`
	StartedAt  time.Time            `json:"startedAt" yaml:"startedAt"`
	FinishedAt *time.Time           `json:"finishedAt,omitempty" yaml:"finishedAt,omitempty"`
	RetryCount int64                `json:"retryCount,omitempty" yaml:"retryCount,omitempty"`
}

// SyncOperationResult is the outcome of a sync.
type SyncOperationResult struct {
	Resources []*ResourceResult `json:"resources,omitempty" yaml:"resources,omitempty"`
	Revision  string            `json:"revision" yaml:"revision"`
	Source    ApplicationSource `json:"source,omitempty" yaml:"source,omitempty"`
}

// ResultCode is the per-resource outcome of a sync.
type ResultCode string

const (
	ResultSynced       ResultCode = "Synced"
	ResultSyncFailed   ResultCode = "SyncFailed"
	ResultPruned       ResultCode = "Pruned"
	ResultPruneSkipped ResultCode = "PruneSkipped"
)

// ResourceResult is what a sync did to one resource.
type ResourceResult struct {
	Kind      string     `json:"kind" yaml:"kind"`
	Namespace string     `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name      string     `json:"name" yaml:"name"`
	Status    ResultCode `json:"status" yaml:"status"`
	Message   string     `json:"message,omitempty" yaml:"message,omitempty"`
	SyncPhase string     `json:"syncPhase,omitempty" yaml:"syncPhase,omitempty"`
	// EvalID and JobVersion identify the Nomad evaluation and version a
	// job registration created.
	EvalID     string  `json:"evalId,omitempty" yaml:"evalId,omitempty"`
	JobVersion *uint64 `json:"jobVersion,omitempty" yaml:"jobVersion,omitempty"`
}

// AppProject groups Applications and restricts their sources and
// destinations.
type AppProject struct {
	APIVersion string         `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind       string         `json:"kind,omitempty" yaml:"kind,omitempty"`
	Metadata   ObjectMeta     `json:"metadata" yaml:"metadata"`
	Spec       AppProjectSpec `json:"spec" yaml:"spec"`
}

// AppProjectSpec lists what a project's Applications may use. Globs: * and ?.
type AppProjectSpec struct {
	Description  string                   `json:"description,omitempty" yaml:"description,omitempty"`
	SourceRepos  []string                 `json:"sourceRepos,omitempty" yaml:"sourceRepos,omitempty"`
	Destinations []ApplicationDestination `json:"destinations,omitempty" yaml:"destinations,omitempty"`
}

// Repository is a connected Git repository. Secret material is never
// returned by the API; it lives in Vault (or the store when Vault is off).
type Repository struct {
	Repo     string `json:"repo" yaml:"repo"`
	Name     string `json:"name,omitempty" yaml:"name,omitempty"`
	Type     string `json:"type,omitempty" yaml:"type,omitempty"`
	Project  string `json:"project,omitempty" yaml:"project,omitempty"`
	Username string `json:"username,omitempty" yaml:"username,omitempty"`
	// Password and SSHPrivateKey are write-only.
	Password      string `json:"password,omitempty" yaml:"password,omitempty"`
	SSHPrivateKey string `json:"sshPrivateKey,omitempty" yaml:"sshPrivateKey,omitempty"`
	// Insecure skips TLS / host-key verification.
	Insecure bool `json:"insecure,omitempty" yaml:"insecure,omitempty"`
	// HasCredentials reports whether secret material is stored.
	HasCredentials  bool            `json:"hasCredentials,omitempty" yaml:"hasCredentials,omitempty"`
	ConnectionState ConnectionState `json:"connectionState,omitempty" yaml:"connectionState,omitempty"`
}

// ConnectionState is the result of the last connection attempt.
type ConnectionState struct {
	Status      string     `json:"status,omitempty" yaml:"status,omitempty"`
	Message     string     `json:"message,omitempty" yaml:"message,omitempty"`
	AttemptedAt *time.Time `json:"attemptedAt,omitempty" yaml:"attemptedAt,omitempty"`
}

// Connection statuses.
const (
	ConnectionSuccessful = "Successful"
	ConnectionFailed     = "Failed"
	ConnectionUnknown    = "Unknown"
)

// Event is an Application event (Argo CD shows Kubernetes events).
type Event struct {
	Type      string    `json:"type" yaml:"type"` // Normal or Warning
	Reason    string    `json:"reason" yaml:"reason"`
	Message   string    `json:"message" yaml:"message"`
	Kind      string    `json:"kind,omitempty" yaml:"kind,omitempty"`
	Name      string    `json:"name,omitempty" yaml:"name,omitempty"`
	Namespace string    `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Username  string    `json:"username,omitempty" yaml:"username,omitempty"`
	Count     int       `json:"count,omitempty" yaml:"count,omitempty"`
	FirstSeen time.Time `json:"firstTimestamp" yaml:"firstTimestamp"`
	LastSeen  time.Time `json:"lastTimestamp" yaml:"lastTimestamp"`
}
