package v1alpha1

// LeadWorkerRecord is the C4 ID of the control record that added unit idx
// to a lead's job: the HUD's signed record of the append whose units hold
// it (its request, the lead's act, not the queue's decision), else the
// submission that made the job with its first workers. A worker's chain names it
// (kind lead-worker), so its record says which act of its lead started it.
// "" for a job that is not a lead's, or a unit no record names.
func (j *WorkJob) LeadWorkerRecord(idx int) string {
	if j.Annotations[AnnotationLeadSession] == "" {
		return ""
	}
	for _, a := range j.Status.Appends {
		if a.Control != "" && idx >= int(a.First) && idx < int(a.First+a.Units) {
			return a.Control
		}
	}
	return j.Annotations["c4.avalanche.io/submission-record"]
}
