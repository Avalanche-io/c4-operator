package provenance

import (
	"encoding/json"
	"path"
	"strings"
	"time"
)

// AIUse is a job's declared use of generative models, as written in the
// WorkJob annotation c4.avalanche.io/ai-usage (DESIGN.md section 9.4).
type AIUse struct {
	// Generated is none, assisted (a person's work with model help) or
	// generated (the output is model output).
	Generated string `json:"generated"`
	Model     string `json:"model,omitempty"`
	// Oversight is fully_autonomous, prompt_guided or human_validated
	// (C2PA 2.4's humanOversightLevel).
	Oversight string `json:"oversight,omitempty"`
	// Consent names the consent records that cover the inputs (section
	// 9.4), for example a performer's likeness release.
	Consent []string `json:"consent,omitempty"`
}

func (p *TaskPredicate) ai() *AIUse {
	if len(p.AI) == 0 {
		return nil
	}
	var a AIUse
	if json.Unmarshal(p.AI, &a) != nil {
		return nil
	}
	return &a
}

// IPTC digital source types (C2PA 2.4 section 18.15).
const (
	sourceAlgorithmic = "http://cv.iptc.org/newscodes/digitalsourcetype/algorithmicMedia"
	sourceTrained     = "http://cv.iptc.org/newscodes/digitalsourcetype/trainedAlgorithmicMedia"
	sourceComposite   = "http://cv.iptc.org/newscodes/digitalsourcetype/compositeWithTrainedAlgorithmicMedia"
)

// C2PAManifest is a manifest definition for c2patool or the C2PA SDKs,
// for one file a task wrote: the claim generator, a c2pa.created action
// with the digital source type the job declared (a renderer's output is
// algorithmicMedia; declared model output is trainedAlgorithmicMedia or
// compositeWithTrainedAlgorithmicMedia), the C2PA 2.4 AI disclosure when a
// model was used, and the task record itself as a custom assertion. The
// signing certificate and the hard binding are the signer's: c2patool adds
// them when it embeds the manifest (or writes a .c2pa sidecar for EXR and
// MXF, which define no embedding).
func C2PAManifest(st *Statement, recordID, file, fileID string) map[string]any {
	p := &st.Predicate
	source := sourceAlgorithmic
	var ai map[string]any
	if a := p.ai(); a != nil && a.Generated != "" && a.Generated != "none" {
		source = sourceTrained
		if a.Generated == "assisted" {
			source = sourceComposite
		}
		ai = map[string]any{"modelType": "generative", "modelName": a.Model}
		if a.Oversight != "" {
			ai["contentProfile"] = map[string]any{"humanOversightLevel": a.Oversight}
		}
	}
	action := map[string]any{
		"action":            "c2pa.created",
		"digitalSourceType": source,
		"when":              p.Runtime.FinishedOn.UTC().Format(time.RFC3339),
		"softwareAgent":     map[string]any{"name": "c4-operator " + p.Invocation.Class + " unit", "version": p.Builder.Version},
		"parameters": map[string]any{
			"job":  p.Invocation.Namespace + "/" + p.Invocation.Job,
			"unit": p.Invocation.Unit, "attempt": p.Invocation.Attempt,
		},
	}
	assertions := []map[string]any{
		{"label": "c2pa.actions.v2", "data": map[string]any{"actions": []any{action}}},
		{"label": "io.cccc.task-provenance", "data": map[string]any{
			"record":        recordID,
			"predicateType": PredicateType,
			"file":          map[string]any{"path": file, "c4": fileID},
			"inputs":        p.Inputs,
			"output":        p.Output.View,
			"node":          p.Runtime.Node,
			"images":        p.Runtime.Images,
		}},
	}
	if ai != nil {
		assertions = append(assertions, map[string]any{"label": "c2pa.ai-disclosure", "data": ai})
	}
	return map[string]any{
		"claim_generator_info": []any{map[string]any{"name": "c4-operator", "version": p.Builder.Version}},
		"title":                path.Base(file),
		"assertions":           assertions,
	}
}

// OMCAsset is the file as an OMC v2.8 Asset (JSON): its C4 ID as an
// identifier (OMC has no hash term, so the ID is an identifier with scope
// c4), and provenance naming the task and when it ran. Structural
// characteristics beyond the file name are left to the caller, who knows
// the media type.
func OMCAsset(st *Statement, recordID, file, fileID string) map[string]any {
	p := &st.Predicate
	name := path.Base(file)
	return map[string]any{
		"entityType": "Asset",
		"identifier": []any{
			map[string]any{"identifierScope": "c4", "identifierValue": fileID, "combinedForm": "urn:c4:" + fileID},
		},
		"name":        name,
		"description": "Written by unit " + p.Invocation.Unit + " of " + p.Invocation.Namespace + "/" + p.Invocation.Job,
		"provenance": map[string]any{
			"createdOn":  p.Runtime.FinishedOn.UTC().Format(time.RFC3339),
			"reason":     "render farm task " + strings.TrimSpace(p.Invocation.Class),
			"annotation": "task record " + recordID + " (" + PredicateType + ")",
		},
		"AssetSC": map[string]any{
			"entityType": "AssetSC",
			"fileDetails": map[string]any{
				"fileName": name,
				"filePath": path.Dir(file),
			},
		},
	}
}
