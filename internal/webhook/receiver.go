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

// Package webhook implements an HTTP receiver for registry push events. On a
// matching event it touches the corresponding ImagePolicy objects to trigger an
// immediate re-scan, complementing the interval-based polling.
package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	imagesv1alpha1 "github.com/saphire/image-updater-operator/api/v1alpha1"
	"github.com/saphire/image-updater-operator/internal/registry"
	"github.com/saphire/image-updater-operator/internal/workload"
)

// imageRepoIndex indexes ImagePolicy objects by their canonical repository so a
// pushed repository can be mapped to the policies that scan it.
const imageRepoIndex = "spec.imageRepositoryCanonical"

const triggeredAtAnnotation = workload.AnnotationPrefix + "triggered-at"

// Receiver is a manager Runnable serving registry webhook callbacks.
type Receiver struct {
	Client client.Client
	// Addr is the listen address, e.g. ":9090".
	Addr string
	// Token, when non-empty, is required as a bearer token on every request.
	Token string
}

var _ manager.Runnable = (*Receiver)(nil)
var _ manager.LeaderElectionRunnable = (*Receiver)(nil)

// RegisterIndex registers the repository field index on the manager cache. It
// must be called before the manager starts.
func RegisterIndex(mgr manager.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &imagesv1alpha1.ImagePolicy{}, imageRepoIndex,
		func(o client.Object) []string {
			ip := o.(*imagesv1alpha1.ImagePolicy)
			canon, err := registry.CanonicalRepository(ip.Spec.ImageRepository)
			if err != nil {
				return nil
			}
			return []string{canon}
		})
}

// NeedLeaderElection returns false so the endpoint is served by every replica.
func (s *Receiver) NeedLeaderElection() bool { return false }

// Start runs the HTTP server until the context is cancelled.
func (s *Receiver) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("webhook")

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/", s.handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("starting webhook receiver", "addr", s.Addr)
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

func (s *Receiver) handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx).WithName("webhook")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	provider := strings.TrimPrefix(r.URL.Path, "/webhook/")
	body, err := readLimited(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	repos, err := parseProvider(provider, body)
	if err != nil {
		log.Info("rejected webhook", "provider", provider, "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	triggered, err := s.trigger(ctx, repos)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Info("webhook processed", "provider", provider, "repos", repos, "triggered", triggered)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"triggered": triggered})
}

// trigger finds ImagePolicies scanning any of the given repositories and patches
// a trigger annotation to force an immediate reconcile. It returns the count of
// policies touched.
func (s *Receiver) trigger(ctx context.Context, repos []string) (int, error) {
	seen := map[string]bool{}
	count := 0
	for _, repo := range repos {
		canon, err := registry.CanonicalRepository(repo)
		if err != nil {
			continue
		}
		var list imagesv1alpha1.ImagePolicyList
		if err := s.Client.List(ctx, &list, client.MatchingFields{imageRepoIndex: canon}); err != nil {
			return count, err
		}
		for i := range list.Items {
			ip := &list.Items[i]
			uid := string(ip.UID)
			if seen[uid] {
				continue
			}
			seen[uid] = true

			patch := client.MergeFrom(ip.DeepCopy())
			ann := ip.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann[triggeredAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
			ip.SetAnnotations(ann)
			if err := s.Client.Patch(ctx, ip, patch); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

func (s *Receiver) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

const maxBodyBytes = 1 << 20 // 1 MiB

func readLimited(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
}
