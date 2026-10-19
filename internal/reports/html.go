package reports

import (
	"bytes"
	"html/template"
	"time"
)

// RenderedReport is the fixed content handed to a future manual email provider.
// It contains no account URL, recipient, pixel, or live-data reference.
type RenderedReport struct {
	Subject        string
	HTML           string
	SnapshotSHA256 string
}

type htmlEstimate struct {
	Estimate
	EnergyValue string
	EnergyUnit  string
}

type htmlItem struct {
	Item
	EstimateView *htmlEstimate
}

var reportHTML = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>EnergyLens report</title></head><body>
<main><h1>EnergyLens report</h1>
<p>Report {{.PreviewID}}. Approved snapshot {{.Digest}}. Created {{.CreatedUTC}} UTC.</p>
<p>This report covers selected appliances only. It does not state a whole-home total, measured savings, waste finding, or incentive.</p>
<section aria-labelledby="appliances"><h2 id="appliances">Selected appliances</h2><ol>
{{range .Items}}<li><h3>{{.Category}} ({{.ApplianceID}})</h3><p>{{.Explanation}}</p>
{{if .EstimateView}}<p>{{.EstimateView.Kind}}: {{.EstimateView.EnergyValue}} {{.EstimateView.EnergyUnit}} and ${{.EstimateView.CostUSD}} USD for {{.EstimateView.PeriodStart}} to {{.EstimateView.PeriodEnd}}. This is an estimate, not measured use or a savings claim.</p>
{{else}}<p>No current monthly estimate is available.</p>{{end}}
{{if .Measured}}<p>Measured electricity: {{.Measured.EnergyKWh}} kWh across {{.Measured.BucketCount}} complete daily sensor buckets, {{.Measured.PeriodStart}} to {{.Measured.PeriodEnd}}. Coverage: {{.Measured.Coverage}}; this is not a whole-month or whole-home total.</p>{{else}}<p>No measured electricity summary is available for this appliance.</p>{{end}}</li>{{end}}
</ol></section>
<section aria-labelledby="recommendations"><h2 id="recommendations">Available recommendations</h2>
{{if .Recommendations}}<ol>{{range .Recommendations}}<li><strong>{{.Title}}</strong> ({{.State}}, {{.RuleID}} version {{.RuleVersion}}; appliance {{.ApplianceID}}). {{.Explanation}} No savings amount is established.{{if .EvidenceRefs}} Evidence: <ul>{{range .EvidenceRefs}}<li>{{.Kind}} {{.ID}}, revision {{.Revision}}</li>{{end}}</ul>{{end}}</li>{{end}}</ol>{{else}}<p>No recommendation is available for the selected appliances.</p>{{end}}</section>
<section aria-labelledby="assumptions"><h2 id="assumptions">Scope and assumptions</h2><ul>{{range .Assumptions}}<li>{{.}}</li>{{end}}</ul></section>
<p>This is a fixed copy of the approved snapshot. Forwarded copies cannot be recalled and grant no account access.</p>
</main></body></html>`))

// RenderApprovedHTML accepts only the exact approved immutable snapshot. The
// template autoescapes every snapshot field, including explanations and IDs.
func RenderApprovedHTML(preview Preview, approvedDigest string) (RenderedReport, error) {
	if preview.Stale {
		return RenderedReport{}, ErrStalePreview
	}
	digest, err := snapshotDigest(preview)
	if err != nil {
		return RenderedReport{}, err
	}
	if (preview.SchemaVersion != "manual-preview/v1" && preview.SchemaVersion != "manual-preview/v2") || digest != preview.SnapshotSHA256 || digest != approvedDigest {
		return RenderedReport{}, ErrSnapshotMismatch
	}
	items := make([]htmlItem, 0, len(preview.Items))
	for _, item := range preview.Items {
		view := htmlItem{Item: item}
		if item.Estimate != nil {
			energyValue, energyUnit := item.Estimate.EnergyKWh, "kWh"
			if item.Estimate.Fuel == "natural_gas" {
				energyValue, energyUnit = item.Estimate.EnergyTherms, "therm natural gas"
			}
			view.EstimateView = &htmlEstimate{Estimate: *item.Estimate, EnergyValue: energyValue, EnergyUnit: energyUnit}
		}
		items = append(items, view)
	}
	data := struct {
		PreviewID       string
		Digest          string
		CreatedUTC      string
		Items           []htmlItem
		Assumptions     []string
		Recommendations []ReportRecommendation
	}{preview.ID, digest, preview.CreatedAt.UTC().Format(time.RFC3339), items, preview.Assumptions, preview.Recommendations}
	var output bytes.Buffer
	if err := reportHTML.Execute(&output, data); err != nil {
		return RenderedReport{}, err
	}
	return RenderedReport{Subject: "EnergyLens report", HTML: output.String(), SnapshotSHA256: digest}, nil
}
