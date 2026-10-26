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
	first := -1 // the first append's first unit: those before it are the submission's
	for _, a := range j.Status.Appends {
		if a.Units > 0 && (first < 0 || int(a.First) < first) {
			first = int(a.First)
		}
		if idx >= int(a.First) && idx < int(a.First+a.Units) {
			return a.Control // "" when the append was not signed: no add rather than the wrong one
		}
	}
	if first >= 0 && idx >= first {
		return "" // past the first workers, in no append on record
	}
	return j.Annotations["c4.avalanche.io/submission-record"]
}
