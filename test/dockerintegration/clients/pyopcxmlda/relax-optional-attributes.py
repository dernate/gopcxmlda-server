"""Relaxes two over-strict expressions in pyopcxmlda's response parsers.

Both treat an attribute the OPC XML-DA schema makes optional as
mandatory, and both crash the client outright rather than degrading:

1. Browse: every BrowseElement's xsi:type is read and .replace() called
   on the result unconditionally, so a response without one raises
   AttributeError. Nothing in the protocol asks for it -- BrowseResponse
   declares its Elements children as BrowseElement (Appendix B), a type
   that is not polymorphic, so there is no type for xsi:type to
   disambiguate, and this server does not emit redundant type
   attributes. Every other attribute that parser demands -- Name,
   ItemPath, ItemName, IsItem, HasChildren -- the server does send.

2. GetProperties: the quality property's QualityField attribute is
   indexed directly, so an omitted one raises KeyError. OPCQuality's
   QualityField carries a schema default of "good" (§3.1.5, pp.30-33),
   and the specification is explicit that good quality may omit the
   attribute entirely -- which is what this server does, and what keeps
   a reply carrying one Quality per item from repeating "good" on every
   one of them.

Deliberately surgical: each edit relaxes one expression from "required"
to "optional" and leaves the client's parsing logic intact, since that
logic is the thing under test. Each fails the image build loudly if its
expression is not found, so a version bump cannot silently turn this
into a no-op.
"""

import pathlib
import sys

import pyopcxmlda.client

path = pathlib.Path(pyopcxmlda.client.__file__)
src = path.read_text(encoding="utf-8")

RELAXATIONS = [
    (
        "BrowseElement xsi:type",
        'xsiType=p.get("{http://www.w3.org/2001/XMLSchema-instance}type").replace("xsd:", "")',
        'xsiType=(p.get("{http://www.w3.org/2001/XMLSchema-instance}type") or "").replace("xsd:", "")',
    ),
    (
        "quality property QualityField",
        'properties._replace(quality=value.attrib["QualityField"])',
        'properties._replace(quality=value.attrib.get("QualityField", "good"))',
    ),
]

for what, before, after in RELAXATIONS:
    if src.count(before) != 1:
        sys.exit(
            f"expected exactly one occurrence of the strict {what} expression in {path}, "
            f"found {src.count(before)} -- the pinned pyopcxmlda version changed and this "
            f"patch needs to be re-checked against it"
        )
    src = src.replace(before, after)

path.write_text(src, encoding="utf-8")
print(f"patched {path}: {len(RELAXATIONS)} optional attributes no longer required")
