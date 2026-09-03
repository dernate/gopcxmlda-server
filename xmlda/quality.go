package xmlda

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// QualityField is the OPC quality indicator, per §3.1.5 of the
// specification.
type QualityField string

// Standard QualityField values (16 total, per §3.1.5).
const (
	QualityBad                        QualityField = "bad"
	QualityBadConfigurationError      QualityField = "badConfigurationError"
	QualityBadNotConnected            QualityField = "badNotConnected"
	QualityBadDeviceFailure           QualityField = "badDeviceFailure"
	QualityBadSensorFailure           QualityField = "badSensorFailure"
	QualityBadLastKnownValue          QualityField = "badLastKnownValue"
	QualityBadCommFailure             QualityField = "badCommFailure"
	QualityBadOutOfService            QualityField = "badOutOfService"
	QualityBadWaitingForInitialData   QualityField = "badWaitingForInitialData"
	QualityUncertain                  QualityField = "uncertain"
	QualityUncertainLastUsableValue   QualityField = "uncertainLastUsableValue"
	QualityUncertainSensorNotAccurate QualityField = "uncertainSensorNotAccurate"
	QualityUncertainEUExceeded        QualityField = "uncertainEUExceeded"
	QualityUncertainSubNormal         QualityField = "uncertainSubNormal"
	QualityGood                       QualityField = "good" // wire default
	QualityGoodLocalOverride          QualityField = "goodLocalOverride"
)

// LimitField is the OPC limit indicator, per §3.1.5 of the specification.
type LimitField string

// Standard LimitField values (4 total, per §3.1.5).
const (
	LimitNone     LimitField = "none" // wire default
	LimitLow      LimitField = "low"
	LimitHigh     LimitField = "high"
	LimitConstant LimitField = "constant"
)

// OPCQuality is the quality of an item value, per §3.1.5. The zero
// OPCQuality is Good/None/0, matching the specification's own wire
// defaults (a Good-quality value may omit the QualityField attribute
// entirely — REQ-QUALITY-002).
type OPCQuality struct {
	qualityField *QualityField
	limitField   *LimitField
	vendorField  uint8
}

// NewGoodQuality returns the default Good/None/0 quality.
func NewGoodQuality() OPCQuality { return OPCQuality{} }

// NewQuality returns an OPCQuality with the given fields.
func NewQuality(q QualityField, l LimitField, vendor uint8) OPCQuality {
	return OPCQuality{qualityField: &q, limitField: &l, vendorField: vendor}
}

// QualityField returns q's quality indicator, defaulting to Good if not
// explicitly set (REQ-QUALITY-002).
func (q OPCQuality) QualityField() QualityField {
	if q.qualityField == nil {
		return QualityGood
	}
	return *q.qualityField
}

// LimitField returns q's limit indicator, defaulting to None if not
// explicitly set.
func (q OPCQuality) LimitField() LimitField {
	if q.limitField == nil {
		return LimitNone
	}
	return *q.limitField
}

// VendorField returns q's vendor-specific quality bits (0 by default).
func (q OPCQuality) VendorField() uint8 { return q.vendorField }

// IsGood reports whether q's QualityField is good or goodLocalOverride.
func (q OPCQuality) IsGood() bool {
	qf := q.QualityField()
	return qf == QualityGood || qf == QualityGoodLocalOverride
}

// IsBad reports whether q's QualityField has the "bad" prefix.
func (q OPCQuality) IsBad() bool {
	return strings.HasPrefix(string(q.QualityField()), "bad")
}

// IsUncertain reports whether q's QualityField has the "uncertain" prefix.
func (q OPCQuality) IsUncertain() bool {
	return strings.HasPrefix(string(q.QualityField()), "uncertain")
}

// ResolveValuePresence implements the quality-driven value-presence rule
// from §3.1.5 (REQ-QUALITY-002/003/004): Good and Uncertain quality
// require a Value element to be present (Uncertain still requires a
// "reasonable" value); Bad quality omits the Value entirely unless a
// last-known value is available. This is independent of, and evaluated
// separately from, RequestOptions.ReturnItemTime (which gates Timestamp
// presence, not Value presence) — see docs/architecture/data-flow.md.
func ResolveValuePresence(q OPCQuality, haveLastKnown bool) bool {
	if q.IsBad() {
		return haveLastKnown
	}
	return true
}

// MarshalXML implements xml.Marshaler, matching the observed wire shape
// (testdata/responses/subscribe_680.response.xml):
// <Quality LimitField="none" QualityField="good" VendorField="0" xsi:type="opc:OPCQuality"/>.
func (q OPCQuality) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, typeAttrs(e, start.Attr, QName{Space: Namespace, Local: "OPCQuality"})...)
	if q.qualityField != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "QualityField"}, Value: string(*q.qualityField)})
	}
	if q.limitField != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "LimitField"}, Value: string(*q.limitField)})
	}
	if q.vendorField != 0 {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "VendorField"}, Value: strconv.Itoa(int(q.vendorField))})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
//
// The element is consumed first, before any attribute is validated, so
// that a rejected VendorField still leaves the decoder positioned after
// this element's end tag. ItemValue.UnmarshalXML's own token loop depends
// on that: it records a bad Quality as one item's condition and carries
// on with the next item, which it can only do if the stream is where it
// expects.
func (q *OPCQuality) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	skipErr := d.Skip()
	if v, ok := attrValue(start.Attr, xml.Name{Local: "QualityField"}); ok {
		qf := QualityField(v)
		q.qualityField = &qf
	}
	if v, ok := attrValue(start.Attr, xml.Name{Local: "LimitField"}); ok {
		lf := LimitField(v)
		q.limitField = &lf
	}
	if v, ok := attrValue(start.Attr, xml.Name{Local: "VendorField"}); ok {
		u, err := strconv.ParseUint(strings.TrimSpace(v), 10, 8)
		if err != nil {
			return fmt.Errorf("xmlda: invalid Quality VendorField %q: %w", v, err)
		}
		q.vendorField = uint8(u)
	}
	return skipErr
}
