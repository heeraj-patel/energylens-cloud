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
	AsOfUTC string
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
{{if .EstimateView}}<p>{{.EstimateView.Kind}}: {{.EstimateView.EnergyKWh}} kWh and ${{.EstimateView.CostUSD}} USD for {{.EstimateView.PeriodStart}} to {{.EstimateView.PeriodEnd}}. This is an estimate, not measured use or a savings claim.</p>
<p>Estimate evidence as of {{.EstimateView.AsOfUTC}} UTC.</p><ul>{{range .EstimateView.SourceRefs}}<li>{{.Kind}} {{.ID}}, revision {{.Revision}}</li>{{end}}</ul>
{{else}}<p>No current reviewed monthly estimate is available.</p>{{end}}</li>{{end}}
</ol></section>
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
	if preview.SchemaVersion != "manual-preview/v1" || digest != preview.SnapshotSHA256 || digest != approvedDigest {
		return RenderedReport{}, ErrSnapshotMismatch
	}
	items := make([]htmlItem, 0, len(preview.Items))
	for _, item := range preview.Items {
		view := htmlItem{Item: item}
		if item.Estimate != nil {
			view.EstimateView = &htmlEstimate{Estimate: *item.Estimate, AsOfUTC: item.Estimate.AsOf.UTC().Format(time.RFC3339)}
		}
		items = append(items, view)
	}
	data := struct {
		PreviewID   string
		Digest      string
		CreatedUTC  string
		Items       []htmlItem
		Assumptions []string
	}{preview.ID, digest, preview.CreatedAt.UTC().Format(time.RFC3339), items, preview.Assumptions}
	var output bytes.Buffer
	if err := reportHTML.Execute(&output, data); err != nil {
		return RenderedReport{}, err
	}
	return RenderedReport{Subject: "EnergyLens report", HTML: output.String(), SnapshotSHA256: digest}, nil
}
