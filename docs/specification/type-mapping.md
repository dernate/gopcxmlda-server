# Data Type Mapping: XSD ↔ OPC Variant ↔ Go

Source: §2.7 (pp. 23-26) of the specification. Implemented in package `xmlda` (`value.go`). See
REQ-TYPE-001..007.

## Scalar types

| XSD type (`xsi:type` local name) | OPC Variant | Go type | Parsing rules | Encoding rules | Overflow | Invalid value | Missing value |
|---|---|---|---|---|---|---|---|
| `string` | VT_BSTR | `string` | element text, as-is | element text, XML-escaped by `encoding/xml` | n/a | n/a (any text is valid) | `Value` is `nil` (see "Missing vs. null" below) |
| `boolean` | VT_BOOL | `bool` | `strconv.ParseBool` (accepts `true`/`false`/`1`/`0`) | `"true"`/`"false"` | n/a | `*TypeError`-adjacent decode error, request rejected as malformed | as above |
| `float` | VT_R4 | `float32` | `strconv.ParseFloat(s, 32)` | `strconv.FormatFloat(f, 'g', -1, 32)` | `ParseFloat` returns `±Inf` for out-of-range magnitude — preserved, not an error (IEEE754 defines this) | non-numeric text ⇒ decode error | as above |
| `double` | VT_R8 | `float64` | `strconv.ParseFloat(s, 64)` | `strconv.FormatFloat(f, 'g', -1, 64)` | same as float32 | same | as above |
| `decimal` | VT_CY | `Decimal` (validated string wrapper, not `float64` — VT_CY is fixed-4-decimal and not exactly representable as a binary float) | lexical `xsd:decimal` validation only; `Float64()`/`String()` accessors for callers who accept precision loss | wire text preserved verbatim from `NewDecimal`/`NewDecimalFromFloat64` | not applicable to a string-backed type | lexical validation failure ⇒ decode error | as above |
| `long` | VT_I8 | `int64` | `strconv.ParseInt(s, 10, 64)` | `strconv.FormatInt` | `ParseInt` returns a range error | non-numeric ⇒ decode error | as above |
| `int` | VT_I4 | `int32` | `strconv.ParseInt(s, 10, 32)` | `strconv.FormatInt` | range error | non-numeric ⇒ decode error | as above |
| `short` | VT_I2 | `int16` | `strconv.ParseInt(s, 10, 16)` | `strconv.FormatInt` | range error | non-numeric ⇒ decode error | as above |
| `byte` | VT_I1 | `int8` | `strconv.ParseInt(s, 10, 8)` | `strconv.FormatInt` | range error | non-numeric ⇒ decode error | as above |
| `unsignedLong` | VT_UI8 | `uint64` | `strconv.ParseUint(s, 10, 64)` | `strconv.FormatUint` | range error | non-numeric ⇒ decode error | as above |
| `unsignedInt` | VT_UI4 | `uint32` | `strconv.ParseUint(s, 10, 32)` | `strconv.FormatUint` | range error | non-numeric ⇒ decode error | as above |
| `unsignedShort` | VT_UI2 | `uint16` | `strconv.ParseUint(s, 10, 16)` | `strconv.FormatUint` | range error | non-numeric ⇒ decode error | as above |
| `unsignedByte` | VT_UI1 | `uint8` | `strconv.ParseUint(s, 10, 8)` | `strconv.FormatUint` | range error | non-numeric ⇒ decode error | as above |
| `base64Binary` | VT_UI1\|VT_ARRAY | `[]byte` | `base64.StdEncoding.DecodeString` | `base64.StdEncoding.EncodeToString` | n/a | malformed base64 ⇒ decode error | `nil` slice, distinguishable from a present-but-empty `[]byte{}` |
| `dateTime` | VT_DATE | `time.Time` | RFC3339-with-offset per spec examples (`2019-09-23T16:01:50.576+00:00`); parsed via `time.Parse(time.RFC3339Nano, s)` with a fallback pass tolerant of a `+00:00`-style suffix | `time.Format(time.RFC3339Nano)`, always with explicit offset (UTC emitted as `Z` — spec examples use `+00:00`; both are RFC3339-legal, `Z` is Go's canonical form) | n/a | unparseable text ⇒ decode error (this is exactly the real-world failure captured in `testdata/faults/fault_soap12_invalid_datetime.response.xml`) | pointer/`*Value` is `nil` |
| `time` | VT_DATE | `time.Time` | decoded directly from `xsi:type="xsd:time"` (see OQ-12: this library uses the direct form, not `dateTime`+`ValueTypeQualifier`, for its own encoding; `ItemValue`-level decode logic additionally tolerates the `dateTime`+`ValueTypeQualifier` form from peers, as a WP-4/5 follow-up) | `xsi:type="xsd:time"` | n/a | same as dateTime | same |
| `date` | VT_DATE | `time.Time` | decoded directly from `xsi:type="xsd:date"` (see OQ-12) | `xsi:type="xsd:date"` | n/a | same | same |
| `duration` | VT_BSTR | `string` (lexical ISO-8601 duration, e.g. `"P1D"`) | decoded directly from `xsi:type="xsd:duration"` (see OQ-12) | `xsi:type="xsd:duration"` | n/a | this library does not validate ISO-8601 duration lexical form beyond accepting any string, since the spec transmits it as opaque `VT_BSTR` | same |
| `QName` | (no Variant equivalent) | `QName{Space, Local}` | resolved via `resolveQName` against the document's prefix table | rendered with the appropriate declared prefix (or default namespace) | n/a | unresolvable prefix ⇒ decode error | same |
| `anyType` | VT_VARIANT | `Value` (recursive) | only meaningful as an array element type (`ArrayOfAnyType`) | each element carries its own `xsi:type` | n/a | n/a | n/a |

**Missing vs. null distinction (REQ-TYPE-007, cross-cutting)**: an `ItemValue.Value` field is a `*Value`
pointer. `nil` means "no `<Value>` element present at all" (e.g. Bad quality with no last-known value, or a
write-only item on Read) — distinct from a present `Value` whose payload happens to be the empty string /
zero / an empty array, which is a fully valid value. This mirrors the same nil-vs-empty-string discipline
used for `ItemPath` (REQ-NS/§3.1.2) and is enforced by never constructing a non-nil `*Value` except through
one of the typed constructors.

## Array types

Named `ArrayOf<X>` types, encoded as repeated scalar-typed child elements under the `<Value>` element —
confirmed against the real fixture `testdata/responses/subscribe_680.response.xml`:
`<Value xsi:type="ns1:ArrayOfUnsignedShort"><unsignedShort>0</unsignedShort>...</Value>`.

| Array `xsi:type` | Element type | Go representation | Notes |
|---|---|---|---|
| `ArrayOfByte` | `byte` (signed, VT_I1) | `[]int8` | |
| `ArrayOfShort` | `short` | `[]int16` | |
| `ArrayOfUnsignedShort` | `unsignedShort` | `[]uint16` | confirmed against real fixture |
| `ArrayOfInt` | `int` | `[]int32` | |
| `ArrayOfUnsignedInt` | `unsignedInt` | `[]uint32` | |
| `ArrayOfLong` | `long` | `[]int64` | |
| `ArrayOfUnsignedLong` | `unsignedLong` | `[]uint64` | |
| `ArrayOfFloat` | `float` | `[]float32` | |
| `ArrayOfDecimal` | `decimal` | `[]Decimal` | |
| `ArrayOfDouble` | `double` | `[]float64` | |
| `ArrayOfBoolean` | `boolean` | `[]bool` | |
| `ArrayOfString` | `string` | `[]string` | |
| `ArrayOfDateTime` | `dateTime` | `[]time.Time` | |
| `ArrayOfAnyType` | any (heterogeneous) | `[]Value` | each element independently `xsi:type`-tagged; may itself be `KindArray` (nested arrays) |

**Deliberately absent**: `ArrayOfUnsignedByte` does not exist in the spec — a sequence of unsigned bytes is
always transmitted as scalar `base64Binary`, never as an array type. `Array` provides no
`NewUint8Array`/`Uint8s()` constructor/accessor pair for exactly this reason; attempting to build one is a
compile-time impossibility, not a runtime check.

**Overflow/invalid/missing for arrays**: the same per-element parsing rules as the corresponding scalar type
apply to each child element; one malformed element fails the whole array decode with a `*TypeError`
identifying which index and what was expected (never silently drops or substitutes a zero value).

## Unknown / vendor-specific types

Any `xsi:type` not in the tables above (including a vendor type in a non-standard, non-XSD namespace)
decodes to `Value{Kind: KindUnknown}`, capturing the exact `xsi:type` QName and the verbatim inner XML bytes.
This value round-trips unchanged on re-encode and is never treated as a decode error — see ADR-003.
`Value.Raw()` is the only accessor that succeeds for such a value; every typed accessor returns a
`*TypeError` naming the actual (unknown) type.

## `ReqType`-driven coercion (Read)

A Read request item's `ReqType` (QName) asks the server to return the value coerced to a specific type
rather than the item's native/canonical type. Coercion is implemented as ordinary Go conversions with
explicit range/precision checks (e.g. `float64`→`int32` checked against `math.MinInt32`/`MaxInt32` before
truncating); any conversion this library cannot perform safely returns `E_BADTYPE` for that item rather than
silently truncating or wrapping. `ReqType` empty/missing/`anyType` ⇒ return the canonical type unchanged.
