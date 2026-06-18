/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package dashboard serves a read-only web UI that shows what the operator is
// monitoring: every ImagePolicy with its latest selected tag, and every workload
// that opts in via annotations, with its containers, current versus desired
// image, and write-back method. It is a manager Runnable served on every replica
// and queries the controller's cached client, so it adds no extra API load.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	imagesv1alpha1 "github.com/saphire/image-updater-operator/api/v1alpha1"
	"github.com/saphire/image-updater-operator/internal/workload"
)

//go:embed index.html
var assets embed.FS

// Server is a manager Runnable serving the dashboard and its JSON API.
type Server struct {
	Client client.Client
	// Addr is the listen address, e.g. ":8082".
	Addr string
}

var _ manager.Runnable = (*Server)(nil)
var _ manager.LeaderElectionRunnable = (*Server)(nil)

// NeedLeaderElection returns false so the dashboard is served by every replica.
func (s *Server) NeedLeaderElection() bool { return false }

// Start runs the HTTP server until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("dashboard")

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("/api/overview", s.handleOverview)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: s.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 1)
	go func() {
		log.Info("starting dashboard", "addr", s.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Overview is the payload the UI polls.
type Overview struct {
	GeneratedAt string         `json:"generatedAt"`
	Policies    []PolicyView   `json:"policies"`
	Workloads   []WorkloadView `json:"workloads"`
}

// PolicyView is one ImagePolicy and its current status.
type PolicyView struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	ImageRepository string `json:"imageRepository"`
	Rule            string `json:"rule"`
	Interval        string `json:"interval"`
	UpdateMode      string `json:"updateMode"`
	Suspended       bool   `json:"suspended"`
	LatestTag       string `json:"latestTag"`
	LatestImage     string `json:"latestImage"`
	LastScanTime    string `json:"lastScanTime"`
	ScannedTags     int    `json:"scannedTags"`
	Ready           string `json:"ready"`
	Reason          string `json:"reason"`
	Message         string `json:"message"`
	WorkloadCount   int    `json:"workloadCount"`
}

// WorkloadView is one annotated workload and its tracked containers.
type WorkloadView struct {
	Namespace  string          `json:"namespace"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	WriteBack  string          `json:"writeBack"`
	GitRepo    string          `json:"gitRepo,omitempty"`
	GitBranch  string          `json:"gitBranch,omitempty"`
	GitTarget  string          `json:"gitTarget,omitempty"`
	Containers []ContainerView `json:"containers"`
}

// ContainerView is one container's binding and update state.
type ContainerView struct {
	Name         string `json:"name"`
	Policy       string `json:"policy"`
	CurrentImage string `json:"currentImage"`
	DesiredImage string `json:"desiredImage"`
	UpToDate     bool   `json:"upToDate"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	policies, byKey, err := s.policies(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	workloads, counts, err := s.workloads(ctx, byKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range policies {
		policies[i].WorkloadCount = counts[policies[i].Namespace+"/"+policies[i].Name]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Overview{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Policies:    policies,
		Workloads:   workloads,
	})
}

// policies lists every ImagePolicy and returns the views plus a lookup of
// namespace/name to the policy's latest selected image.
func (s *Server) policies(ctx context.Context) ([]PolicyView, map[string]string, error) {
	var list imagesv1alpha1.ImagePolicyList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, nil, err
	}
	views := make([]PolicyView, 0, len(list.Items))
	byKey := map[string]string{}
	for i := range list.Items {
		ip := &list.Items[i]
		ready, reason, message := readyCondition(ip)
		v := PolicyView{
			Namespace:       ip.Namespace,
			Name:            ip.Name,
			ImageRepository: ip.Spec.ImageRepository,
			Rule:            ruleOf(&ip.Spec),
			Interval:        durationOrDefault(ip.Spec.Interval.Duration, "5m"),
			UpdateMode:      updateModeOrDefault(string(ip.Spec.UpdateMode)),
			Suspended:       ip.Spec.Suspend,
			LatestTag:       ip.Status.LatestTag,
			LatestImage:     ip.Status.LatestImage,
			ScannedTags:     ip.Status.ScannedTags,
			Ready:           ready,
			Reason:          reason,
			Message:         message,
		}
		if ip.Status.LastScanTime != nil {
			v.LastScanTime = ip.Status.LastScanTime.UTC().Format(time.RFC3339)
		}
		views = append(views, v)
		byKey[ip.Namespace+"/"+ip.Name] = ip.Status.LatestImage
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Namespace != views[j].Namespace {
			return views[i].Namespace < views[j].Namespace
		}
		return views[i].Name < views[j].Name
	})
	return views, byKey, nil
}

// workloads lists every supported kind, keeps those that opt in via a policy
// annotation, and returns the views plus per-policy reference counts.
func (s *Server) workloads(ctx context.Context, latestByKey map[string]string) ([]WorkloadView, map[string]int, error) {
	views := []WorkloadView{} // non-nil so the JSON is [] not null when empty
	counts := map[string]int{}

	for _, adapter := range workload.Adapters() {
		list := adapter.NewList()
		if err := s.Client.List(ctx, list); err != nil {
			return nil, nil, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				continue
			}
			// Skip objects owned by a controller (a ReplicaSet behind a
			// Deployment, a Pod behind a ReplicaSet, a Job behind a CronJob).
			// Kubernetes copies annotations onto these, but the top-level owner
			// is the resource the user opted in, so show only that.
			if metav1.GetControllerOf(obj) != nil {
				continue
			}
			ann := obj.GetAnnotations()
			policies := workload.ContainerPolicies(ann)
			if len(policies) == 0 {
				continue
			}

			images := map[string]string{}
			if spec := adapter.PodSpec(obj); spec != nil {
				for _, c := range workload.AllContainers(spec) {
					images[c.Name] = c.Image
				}
			}

			cfg := workload.GitSettings(ann)
			wv := WorkloadView{
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(),
				Kind:      adapter.Name,
				WriteBack: string(workload.WriteBack(ann)),
			}
			if workload.WriteBack(ann) == workload.MethodGit {
				wv.GitRepo, wv.GitBranch, wv.GitTarget = cfg.Repo, cfg.Branch, cfg.Target
			}
			for cname, pname := range policies {
				counts[obj.GetNamespace()+"/"+pname]++
				desired := latestByKey[obj.GetNamespace()+"/"+pname]
				current := images[cname]
				wv.Containers = append(wv.Containers, ContainerView{
					Name:         cname,
					Policy:       pname,
					CurrentImage: current,
					DesiredImage: desired,
					UpToDate:     desired != "" && current == desired,
				})
			}
			sort.Slice(wv.Containers, func(i, j int) bool { return wv.Containers[i].Name < wv.Containers[j].Name })
			views = append(views, wv)
		}
	}

	sort.Slice(views, func(i, j int) bool {
		if views[i].Namespace != views[j].Namespace {
			return views[i].Namespace < views[j].Namespace
		}
		if views[i].Kind != views[j].Kind {
			return views[i].Kind < views[j].Kind
		}
		return views[i].Name < views[j].Name
	})
	return views, counts, nil
}

func readyCondition(ip *imagesv1alpha1.ImagePolicy) (status, reason, message string) {
	c := meta.FindStatusCondition(ip.Status.Conditions, "Ready")
	if c == nil {
		return "Unknown", "", ""
	}
	return string(c.Status), c.Reason, c.Message
}

func ruleOf(spec *imagesv1alpha1.ImagePolicySpec) string {
	switch {
	case spec.Policy.Semver != nil:
		return "semver " + spec.Policy.Semver.Range
	case spec.Policy.Regex != nil:
		return "regex " + spec.Policy.Regex.Pattern
	case spec.Policy.Numerical != nil:
		return "numerical"
	case spec.Policy.Alphabetical != nil:
		return "alphabetical"
	default:
		return "unset"
	}
}

func durationOrDefault(d time.Duration, def string) string {
	if d == 0 {
		return def
	}
	return d.String()
}

func updateModeOrDefault(mode string) string {
	if mode == "" {
		return string(imagesv1alpha1.UpdateModeAutomatic)
	}
	return mode
}
