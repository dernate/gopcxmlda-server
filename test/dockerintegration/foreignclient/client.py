"""Drive an OPC XML-DA server with a client built from the specification.

This script shares no code, no types and no assumptions with the server
under test. It builds its proxy from testdata/schema/opcxmlda.wsdl — the
WSDL transcribed from the specification's own appendix — using zeep,
which parses the schema strictly and rejects a response that does not
match it. That is the check the two Go integration suites cannot make:
both of them drive the server with the reference client written by the
same author, which proves the two agree with each other.

Every check writes one "CHECK <name> <ok|fail> <detail>" line to stdout so
the Go test can attribute a failure without parsing tracebacks. The exit
code is 0 only if every check passed.
"""

import os
import sys
import datetime

from zeep import Client, Settings, xsd
from zeep.exceptions import Fault, ValidationError
from zeep.transports import Transport

ENDPOINT = os.environ.get("OPCXMLDA_ENDPOINT", "http://localhost:8080/")
NS = "http://opcfoundation.org/webservices/XMLDA/1.0/"

MOTOR_SPEED = "Plant/BuildingA/Line1/Motor1/Speed"
TANK_LEVEL = "Plant/BuildingB/Tank1/Level"
TANK_VALVE = "Plant/BuildingB/Tank1/Valve"
TANK_LABEL = "Plant/BuildingB/Tank1/Label"
TANK_SENSORS = "Plant/BuildingB/Tank1/Sensors"
UNKNOWN = "Plant/No/Such/Item"

failures = []


def check(name, ok, detail=""):
    print("CHECK %s %s %s" % (name, "ok" if ok else "fail", detail))
    if not ok:
        failures.append(name)
    return ok


def make_client():
    # strict=True is the default and is the point: zeep then refuses a
    # response whose shape the schema does not allow, rather than
    # silently ignoring the parts it did not expect. Go's own
    # encoding/xml is lenient in exactly those places.
    settings = Settings(strict=True, xml_huge_tree=False)
    client = Client("opcxmlda.wsdl", settings=settings, transport=Transport(timeout=30))
    service = client.create_service("{%s}Service" % NS, ENDPOINT)
    return client, service


def opts(client, **overrides):
    """RequestOptions with every echo turned on, so the reply carries the
    fields whose presence rules this exercises."""
    t = client.get_type("{%s}RequestOptions" % NS)
    kwargs = dict(
        ReturnErrorText=True,
        ReturnDiagnosticInfo=True,
        ReturnItemTime=True,
        ReturnItemPath=True,
        ReturnItemName=True,
        LocaleID="en-US",
        ClientRequestHandle="foreign-client",
    )
    kwargs.update(overrides)
    return t(**kwargs)


def item_list(client, names, list_type, item_type, **list_attrs):
    items = [client.get_type(item_type)(ItemName=n) for n in names]
    return client.get_type(list_type)(Items=items, **list_attrs)


def run():
    client, svc = make_client()
    check("wsdl-parsed", True, "8 operations bound from the specification's WSDL")

    # ---------------- GetStatus ----------------
    st = svc.GetStatus(LocaleID="en-US", ClientRequestHandle="fc-status")
    check("getstatus-serverstate", st.GetStatusResult.ServerState is not None,
          "ServerState=%s" % st.GetStatusResult.ServerState)
    check("getstatus-replybase-times",
          st.GetStatusResult.RcvTime is not None and st.GetStatusResult.ReplyTime is not None
          and st.GetStatusResult.RcvTime <= st.GetStatusResult.ReplyTime,
          "RcvTime=%s ReplyTime=%s" % (st.GetStatusResult.RcvTime, st.GetStatusResult.ReplyTime))
    check("getstatus-echoes-handle", st.GetStatusResult.ClientRequestHandle == "fc-status",
          repr(st.GetStatusResult.ClientRequestHandle))
    check("getstatus-product-version", bool(st.Status.ProductVersion), st.Status.ProductVersion)
    check("getstatus-interface-version",
          "XML_DA_Version_1_0" in (st.Status.SupportedInterfaceVersions or []),
          str(st.Status.SupportedInterfaceVersions))

    # ---------------- Read ----------------
    rd = svc.Read(
        Options=opts(client),
        ItemList=item_list(client, [TANK_LEVEL, TANK_SENSORS, UNKNOWN],
                           "{%s}ReadRequestItemList" % NS, "{%s}ReadRequestItem" % NS),
    )
    items = rd.RItemList.Items
    check("read-item-count", len(items) == 3, "got %d" % len(items))
    check("read-order-preserved",
          [i.ItemName for i in items] == [TANK_LEVEL, TANK_SENSORS, UNKNOWN],
          str([i.ItemName for i in items]))
    scalar = items[0]
    check("read-scalar-has-value", scalar.Value is not None, repr(scalar.Value))
    check("read-scalar-has-timestamp", scalar.Timestamp is not None, str(scalar.Timestamp))
    check("read-scalar-quality-good",
          scalar.Quality is None or scalar.Quality.QualityField == "good",
          scalar.Quality.QualityField if scalar.Quality is not None else "(absent)")
    check("read-array-has-values", items[1].Value is not None, repr(items[1].Value)[:60])
    bad = items[2]
    check("read-unknown-item-resultid", bad.ResultID is not None, str(bad.ResultID))
    # The failing item must state its quality rather than leave the
    # schema's default (good) to speak for it — §2.6 p.22.
    check("read-unknown-item-quality-bad",
          bad.Quality is not None and bad.Quality.QualityField == "bad",
          bad.Quality.QualityField if bad.Quality is not None else "(absent)")
    check("read-errors-carry-text",
          bool(rd.Errors) and all(e.Text for e in rd.Errors),
          "%d entries" % len(rd.Errors or []))

    # ---------------- Write, then read back ----------------
    #
    # The value has to be wrapped in an explicitly-typed AnyObject, and
    # that is not a quirk of zeep: the schema declares ItemValue's <Value>
    # with no type at all, which in XSD means anyType, so the element's
    # xsi:type IS the type — there is nowhere else for it to come from. A
    # generated client that passes a bare Python float sends <Value> with
    # no xsi:type, and the server answers E_BADTYPE, correctly. Any
    # WSDL-generated client faces this; it is worth knowing before writing
    # one.
    def typed(local, value):
        return xsd.AnyObject(xsd.builtins.default_types["{http://www.w3.org/2001/XMLSchema}" + local], value)

    wr_item = client.get_type("{%s}ItemValue" % NS)(
        ItemName=MOTOR_SPEED,
        Value=typed("double", 1500.0),
    )
    wr = svc.Write(
        ReturnValuesOnReply=True,
        Options=opts(client),
        ItemList=client.get_type("{%s}WriteRequestItemList" % NS)(Items=[wr_item]),
    )
    written = wr.RItemList.Items[0]
    check("write-accepted", written.ResultID is None or "S_" in str(written.ResultID),
          str(written.ResultID))
    rb = svc.Read(
        Options=opts(client),
        ItemList=item_list(client, [MOTOR_SPEED], "{%s}ReadRequestItemList" % NS,
                           "{%s}ReadRequestItem" % NS),
    )
    check("write-read-back", float(rb.RItemList.Items[0].Value) == 1500.0,
          repr(rb.RItemList.Items[0].Value))

    # A clamped write must come back as a SUCCESS code carrying a value,
    # not as an error that discards it.
    clamp_item = client.get_type("{%s}ItemValue" % NS)(ItemName=MOTOR_SPEED, Value=typed("double", 99999.0))
    cl = svc.Write(
        ReturnValuesOnReply=True,
        Options=opts(client),
        ItemList=client.get_type("{%s}WriteRequestItemList" % NS)(Items=[clamp_item]),
    )
    clamped = cl.RItemList.Items[0]
    check("write-clamp-is-success", clamped.ResultID is not None and "S_CLAMP" in str(clamped.ResultID),
          str(clamped.ResultID))

    # ---------------- Browse ----------------
    root = svc.Browse(ClientRequestHandle="fc-browse", ReturnErrorText=True)
    names = [e.Name for e in (root.Elements or [])]
    check("browse-root", names == ["Plant"], str(names))
    lvl2 = svc.Browse(ItemName="Plant", ClientRequestHandle="fc-browse", ReturnErrorText=True)
    check("browse-nested", len(lvl2.Elements or []) >= 2,
          str([e.Name for e in (lvl2.Elements or [])]))
    check("browse-haschildren-flag",
          all(e.HasChildren is not None for e in (lvl2.Elements or [])), "")

    # ---------------- GetProperties ----------------
    gp = svc.GetProperties(
        ItemIDs=[client.get_type("{%s}ItemIdentifier" % NS)(ItemName=TANK_LEVEL)],
        ReturnAllProperties=True, ReturnPropertyValues=True, ReturnErrorText=True,
        ClientRequestHandle="fc-props",
    )
    props = gp.PropertyLists[0].Properties
    check("getproperties-returns-properties", len(props) > 0, "%d" % len(props))
    check("getproperties-names-qualified",
          all(p.Name is not None for p in props), "")

    # ---------------- Subscribe / PolledRefresh / Cancel ----------------
    sub_item = client.get_type("{%s}SubscribeRequestItem" % NS)(
        ItemName=TANK_LEVEL, ClientItemHandle="fc-sub", RequestedSamplingRate=200,
    )
    sub = svc.Subscribe(
        ReturnValuesOnReply=True, SubscriptionPingRate=10000,
        Options=opts(client),
        ItemList=client.get_type("{%s}SubscribeRequestItemList" % NS)(Items=[sub_item]),
    )
    handle = sub.ServerSubHandle
    check("subscribe-issues-handle", bool(handle), repr(handle))
    check("subscribe-revised-rate",
          sub.RItemList is not None and sub.RItemList.Items
          and sub.RItemList.Items[0].RevisedSamplingRate is not None,
          str(sub.RItemList.Items[0].RevisedSamplingRate) if sub.RItemList and sub.RItemList.Items else "-")

    hold = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=2)
    pr = svc.SubscriptionPolledRefresh(
        ServerSubHandles=[handle], HoldTime=hold, WaitTime=3000,
        ReturnAllItems=False, Options=opts(client),
    )
    delivered = sum(len(l.Items or []) for l in (pr.RItemList or []))
    check("polledrefresh-delivers-changes", delivered > 0, "%d item updates" % delivered)
    check("polledrefresh-no-invalid-handles", not pr.InvalidServerSubHandles,
          str(pr.InvalidServerSubHandles))

    all_items = svc.SubscriptionPolledRefresh(
        ServerSubHandles=[handle], ReturnAllItems=True, Options=opts(client),
    )
    check("polledrefresh-returnallitems",
          sum(len(l.Items or []) for l in (all_items.RItemList or [])) > 0, "")

    cancel = svc.SubscriptionCancel(ServerSubHandle=handle, ClientRequestHandle="fc-cancel")
    check("cancel-echoes-handle", cancel == "fc-cancel" or cancel is None, repr(cancel))

    # A cancelled handle must be reported as invalid, not silently ignored.
    # With every requested handle invalid there is no reply shape left to
    # put InvalidServerSubHandles in, so the specification's answer is a
    # whole-operation fault (§3.6, E_NOSUBSCRIPTION). Either outcome is
    # correct; what must not happen is a cancelled handle continuing to
    # work.
    try:
        after = svc.SubscriptionPolledRefresh(ServerSubHandles=[handle], Options=opts(client))
        check("polledrefresh-after-cancel",
              bool(after.InvalidServerSubHandles), str(after.InvalidServerSubHandles))
    except Fault as f:
        code = str(getattr(f, "code", "")) + " " + str(f)
        check("polledrefresh-after-cancel", "E_NOSUBSCRIPTION" in code, code.strip())

    # ---------------- Faults ----------------
    try:
        svc.Read(Options=opts(client),
                 ItemList=item_list(client, [], "{%s}ReadRequestItemList" % NS,
                                    "{%s}ReadRequestItem" % NS))
        check("empty-itemlist-is-served", True, "empty request answered without a fault")
    except Fault as f:
        check("empty-itemlist-is-served", False, "faulted: %s" % f)


def main():
    try:
        run()
    except ValidationError as e:
        # zeep raising this means the server sent something the
        # specification's own schema does not allow.
        check("schema-validation", False, "response rejected by the schema: %s" % e)
    except Exception as e:  # noqa: BLE001 - the Go test wants the reason, not a traceback
        check("client-run", False, "%s: %s" % (type(e).__name__, e))

    if failures:
        print("FAILED %d check(s): %s" % (len(failures), ", ".join(failures)))
        return 1
    print("ALL CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
