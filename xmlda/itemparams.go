package xmlda

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// ItemParams is the set of parameters the specification allows to be set
// at the request level, the item-list level, or the individual item level
// (§3.1.1, p.27; the "hierarchical parameters"), with the most specific
// non-nil value winning. All fields are pointers for exactly that reason:
// nil means "not specified at this level, inherit from the level above",
// while — for ItemPath specifically — a non-nil pointer to "" is an
// explicit override to the empty string (§3.1.2, p.28: null vs. empty
// string is meaningful for ItemPath), not "absent".
//
// ItemParams has no xml struct tags of its own: ReqType is a QName-shaped
// attribute value that needs decoder-scoped prefix resolution
// (resolveQName), which plain struct-tag decoding cannot perform (see
// ADR-004). Callers extract/render these attributes via
// decodeItemParamsAttrs/encodeItemParamsAttrs below, from within their own
// element's custom UnmarshalXML/MarshalXML.
type ItemParams struct {
	ItemPath *string
	ReqType  *QName
	// MaxAge and RequestedSamplingRate are milliseconds, and both are
	// xsd:int on the wire — signed and 32-bit. They are modeled that way
	// rather than as unsigned values so a client sending a negative
	// number (a common spelling of "no preference") is decoded and then
	// normalized by the server, instead of failing the whole request with
	// a parse fault for input the schema permits.
	MaxAge                *int32
	Deadband              *float64
	RequestedSamplingRate *int32
	EnableBuffering       *bool
}

// MergeItemParams applies request < list < item precedence, left to
// right: each non-nil field in a later argument overrides the
// corresponding field from an earlier one. A field left nil by every
// argument stays nil, meaning "use the server's own default" (REQ-READ-001).
func MergeItemParams(levels ...ItemParams) ItemParams {
	var out ItemParams
	for _, lvl := range levels {
		if lvl.ItemPath != nil {
			out.ItemPath = lvl.ItemPath
		}
		if lvl.ReqType != nil {
			out.ReqType = lvl.ReqType
		}
		if lvl.MaxAge != nil {
			out.MaxAge = lvl.MaxAge
		}
		if lvl.Deadband != nil {
			out.Deadband = lvl.Deadband
		}
		if lvl.RequestedSamplingRate != nil {
			out.RequestedSamplingRate = lvl.RequestedSamplingRate
		}
		if lvl.EnableBuffering != nil {
			out.EnableBuffering = lvl.EnableBuffering
		}
	}
	return out
}

// decodeItemParamsAttrs extracts whichever hierarchical parameters are
// present in attrs into an ItemParams, resolving ReqType's QName value
// against the element's own declarations first and then d's
// whole-document scope. Fields absent from attrs are left nil.
//
// The returned error is always an *ItemDecodeError, and the returned
// ItemParams still carries whatever was parsed before the failure — so an
// item-level caller can report the condition as that item's own ResultID
// while still echoing its ItemPath back (see ItemDecodeError). A
// list-level or request-level caller, where the same attribute applies to
// every item at once, instead propagates it as a whole-operation fault.
func decodeItemParamsAttrs(d *xml.Decoder, attrs []xml.Attr) (ItemParams, error) {
	var p ItemParams
	if v, ok := attrValue(attrs, xml.Name{Local: "ItemPath"}); ok {
		p.ItemPath = &v
	}
	if v, ok := attrValue(attrs, xml.Name{Local: "ReqType"}); ok {
		qn, err := resolveQNameIn(d, attrs, v)
		if err != nil {
			// E_BADTYPE, not E_FAIL: a ReqType this server cannot resolve
			// to a type IS the specification's bad-type condition
			// (§3.1.3), and it is what a client gets for an unsupported
			// but resolvable type too — so the two spellings of "I cannot
			// give you that type" report identically.
			return p, &ItemDecodeError{Field: "ReqType", Code: ErrBadType, Err: err}
		}
		p.ReqType = &qn
	}
	if v, ok := attrValue(attrs, xml.Name{Local: "MaxAge"}); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return p, &ItemDecodeError{Field: "MaxAge", Code: ErrFail,
				Err: fmt.Errorf("%q is not a valid xsd:int: %w", v, err)}
		}
		n32 := int32(n)
		p.MaxAge = &n32
	}
	if v, ok := attrValue(attrs, xml.Name{Local: "Deadband"}); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return p, &ItemDecodeError{Field: "Deadband", Code: ErrFail,
				Err: fmt.Errorf("%q is not a valid xsd:float: %w", v, err)}
		}
		p.Deadband = &f
	}
	if v, ok := attrValue(attrs, xml.Name{Local: "RequestedSamplingRate"}); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return p, &ItemDecodeError{Field: "RequestedSamplingRate", Code: ErrFail,
				Err: fmt.Errorf("%q is not a valid xsd:int: %w", v, err)}
		}
		n32 := int32(n)
		p.RequestedSamplingRate = &n32
	}
	if v, ok := attrValue(attrs, xml.Name{Local: "EnableBuffering"}); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return p, &ItemDecodeError{Field: "EnableBuffering", Code: ErrFail,
				Err: fmt.Errorf("%q is not a valid xsd:boolean: %w", v, err)}
		}
		p.EnableBuffering = &b
	}
	return p, nil
}

// encodeItemParamsAttrs renders p's set fields as xml.Attr, for a caller
// assembling its own StartElement. Fields left nil in p are omitted.
func encodeItemParamsAttrs(p ItemParams) []xml.Attr {
	var attrs []xml.Attr
	if p.ItemPath != nil {
		attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "ItemPath"}, Value: *p.ItemPath})
	}
	if p.ReqType != nil {
		attrs = append(attrs, qnameAttr(attrs, "ReqType", *p.ReqType)...)
	}
	if p.MaxAge != nil {
		attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "MaxAge"}, Value: strconv.FormatInt(int64(*p.MaxAge), 10)})
	}
	if p.Deadband != nil {
		attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "Deadband"}, Value: strconv.FormatFloat(*p.Deadband, 'g', -1, 64)})
	}
	if p.RequestedSamplingRate != nil {
		attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "RequestedSamplingRate"}, Value: strconv.FormatInt(int64(*p.RequestedSamplingRate), 10)})
	}
	if p.EnableBuffering != nil {
		attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "EnableBuffering"}, Value: strconv.FormatBool(*p.EnableBuffering)})
	}
	return attrs
}

// marshalRequestItem encodes the ItemParams/ItemName/ClientItemHandle
// attributes shared by ReadRequestItem and SubscribeRequestItem — every
// per-item request element that carries just hierarchical params plus
// those two identifying attributes and no child content.
func marshalRequestItem(e *xml.Encoder, start xml.StartElement, params ItemParams, itemName, clientItemHandle string) error {
	start.Attr = mergeAttrs(start.Attr, encodeItemParamsAttrs(params)...)
	if itemName != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemName"}, Value: itemName})
	}
	if clientItemHandle != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ClientItemHandle"}, Value: clientItemHandle})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// decodeRequestItem is marshalRequestItem's decode counterpart, shared by
// ReadRequestItem and SubscribeRequestItem.
//
// It returns two errors, and the distinction is the whole point:
// decodeErr is a per-item condition the caller stores in the item's
// DecodeErr field for the server layer to turn into a ResultID, while err
// is a structural XML failure that genuinely cannot be attributed to one
// item. This function always consumes start's element either way, so a
// caller's own token loop stays in sync across a rejected item.
func decodeRequestItem(d *xml.Decoder, start xml.StartElement) (params ItemParams, itemName, clientItemHandle string, decodeErr, err error) {
	params, decodeErr = decodeItemParamsAttrs(d, start.Attr)
	itemName, _ = attrValue(start.Attr, xml.Name{Local: "ItemName"})
	clientItemHandle, _ = attrValue(start.Attr, xml.Name{Local: "ClientItemHandle"})
	return params, itemName, clientItemHandle, decodeErr, d.Skip()
}

// xmlUnmarshalerPtr is implemented by *T for every item type used inside a
// repeated "Items"-style element (ReadRequestItem, SubscribeRequestItem,
// ItemValue, SubscribeItemValue).
type xmlUnmarshalerPtr[T any] interface {
	*T
	UnmarshalXML(d *xml.Decoder, start xml.StartElement) error
}

// decodeRepeatedElements reads start's children, decoding each one named
// elemName via its own UnmarshalXML and skipping everything else, until
// start's matching EndElement. It is the shared decode loop behind
// ReadItemList, SubscribeItemList, WriteItemList, and
// SubscribeReplyItemList's UnmarshalXML.
func decodeRepeatedElements[T any, PT xmlUnmarshalerPtr[T]](d *xml.Decoder, elemName, errContext string) ([]T, error) {
	var items []T
	for {
		tok, err := d.Token()
		if err != nil {
			return items, fmt.Errorf("xmlda: decoding %s: %w", errContext, err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return items, nil
		case xml.StartElement:
			if t.Name.Local != elemName {
				if err := d.Skip(); err != nil {
					return items, err
				}
				continue
			}
			var item T
			if err := PT(&item).UnmarshalXML(d, t); err != nil {
				return items, err
			}
			items = append(items, item)
		}
	}
}
