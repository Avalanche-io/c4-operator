package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A host runner's machine, as the farm sees it. A host runner reads its own
// pods here, never from the API server, so its token reads nothing of
// anyone else's work; and a person enrolls a machine as theirs here, with
// their own sign-in, so the machine's owners are a person's signed act and
// never the runner's own word.

// hostsNamespace holds each host runner's account (c4-hosts/<node>).
const hostsNamespace = "c4-hosts"

// hostTaint is on every host runner's Node.
const hostTaint = "c4.avalanche.io/host-runner"

// hostLabel names a host runner's Node; its pool selects it.
const hostLabel = "c4.avalanche.io/host"

// nodeOwnersOf reads a Node's owners (c4v1.AnnotationNodeOwners, a JSON
// list of names, so no character in a name splits an owner). A value that
// does not parse names no one.
func nodeOwnersOf(n *corev1.Node) []string {
	var list []string
	if json.Unmarshal([]byte(n.Annotations[c4v1.AnnotationNodeOwners]), &list) != nil {
		return nil
	}
	var out []string
	for _, o := range list {
		if o = strings.TrimSpace(o); o != "" && !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	sort.Strings(out)
	return out
}

// handleHostPods is GET /nodes/{node}/pods: the unit pods bound to a host
// runner's Node, for that runner alone, each with what the runner needs of
// its job: its owner (whose work it is), its UID (so a job made again under
// the same name is another) and the grant it was submitted under.
func (s *Server) handleHostPods(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	node := r.PathValue("node")
	if host, ok := strings.CutPrefix(v.User, HostAccountPrefix); !ok || host != node {
		fail(w, http.StatusForbidden, "only node %s's own host runner reads its pods", node)
		return
	}
	ctx := r.Context()
	var pods corev1.PodList
	if err := s.Client.List(ctx, &pods, client.HasLabels{c4v1.LabelJob}); err != nil {
		fail(w, http.StatusServiceUnavailable, "the farm's pods: %v", err)
		return
	}
	out := api.HostPods{Node: node, Pods: []api.HostPod{}}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != node {
			continue
		}
		raw, err := json.Marshal(p)
		if err != nil {
			continue
		}
		hp := api.HostPod{Pod: raw}
		if ref := metav1.GetControllerOf(p); ref != nil && ref.Kind == "WorkJob" {
			var j c4v1.WorkJob
			if err := s.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: ref.Name}, &j); err == nil && j.UID == ref.UID {
				hp.Job = &api.HostPodJob{Name: j.Name, UID: string(j.UID), Owner: j.Annotations[c4v1.AnnotationOwner], Grant: j.Annotations[c4v1.AnnotationGrant]}
			}
		}
		out.Pods = append(out.Pods, hp)
	}
	writeJSON(w, http.StatusOK, out)
}
