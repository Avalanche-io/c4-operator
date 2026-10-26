package v1alpha1

// LeadWorkerRecord is the C4 ID of the control record that added unit idx
// to a lead's job: the append whose units hold it, else the submission
// that made the job with its first workers. A worker's chain names it
// (kind lead-worker), so its record says which act of its lead started it.
// "" for a job that is not a lead's, or a unit no record names.
func (j *WorkJob) LeadWorkerRecord(idx int) string {
	if j.Annotations[AnnotationLeadSession] == "" {
		return ""
	}
	for _, a := range j.Status.Appends {
		if a.Record != "" && idx >= int(a.First) && idx < int(a.First+a.Units) {
			return a.Record
		}
	}
	return j.Annotations["c4.avalanche.io/submission-record"]
}
