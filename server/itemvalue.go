package server

import (
	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// buildItemValue constructs the wire ItemValue for one item's outcome,
// applying the quality-driven Value/Timestamp presence rule
// (xmlda.ResolveValuePresence) and the RequestOptions gating for
// ItemName/ItemPath/Timestamp/DiagnosticInfo — the single place this
// logic lives so every operation's response construction applies it
// identically (docs/architecture/data-flow.md). haveSample is false when
// the backend produced no sample at all for this item (e.g. the item is
// write-only, or the whole item was invalid).
func buildItemValue(ref backend.ItemRef, clientItemHandle string, sample backend.ItemSample, haveSample bool, resultID xmlda.ErrorCode, diagnosticInfo string, opts xmlda.RequestOptions) xmlda.ItemValue {
	iv := xmlda.ItemValue{
		ClientItemHandle: clientItemHandle,
		ResultID:         resultID,
	}
	if opts.ReturnItemNameOrDefault() {
		iv.ItemName = ref.ItemName
	}
	if opts.ReturnItemPathOrDefault() {
		path := ref.ItemPath
		iv.ItemPath = &path
	}
	if opts.ReturnDiagnosticInfoOrDefault() {
		iv.DiagnosticInfo = diagnosticInfo
	}
	if !haveSample {
		return iv
	}
	iv.Quality = sample.Quality
	haveLastKnown := !sample.Value.IsNil()
	if xmlda.ResolveValuePresence(sample.Quality, haveLastKnown) {
		v := sample.Value
		iv.Value = &v
		if opts.ReturnItemTimeOrDefault() {
			ts := sample.Timestamp
			iv.Timestamp = &ts
		}
	}
	return iv
}

// errorTextFunc returns the textOf function xmlda.DedupeErrors should use,
// honoring RequestOptions.ReturnErrorText — when false, Errors entries
// still identify which codes occurred (that's the core per-item result
// mechanism, not "error text"), but carry no human-readable Text.
func errorTextFunc(opts xmlda.RequestOptions) func(xmlda.ErrorCode) string {
	if !opts.ReturnErrorTextOrDefault() {
		return func(xmlda.ErrorCode) string { return "" }
	}
	return xmlda.StandardErrorText
}
