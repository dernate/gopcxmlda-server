"""Drives an OPC XML-DA server through NothinRandom/pyopcxmlda.

Prints one "CHECK <name> ok|fail <detail>" line per assertion, so the Go
test can attribute a failure to the thing that failed rather than to an
opaque non-zero exit, and "ALL CHECKS PASSED" at the end so a client that
died halfway is distinguishable from one that found nothing wrong.
"""

import os
import sys
import time
from datetime import datetime, timedelta, timezone
from urllib.parse import urlparse

from pyopcxmlda import Client
from pyopcxmlda.tag import Tag

SPEED = "Plant/BuildingA/Line1/Motor1/Speed"
TEMPERATURE = "Plant/BuildingA/Line1/Motor1/Temperature"
RUNNING = "Plant/BuildingA/Line1/Motor1/Running"
LEVEL = "Plant/BuildingB/Tank1/Level"
LABEL = "Plant/BuildingB/Tank1/Label"
CAPACITY = "Plant/BuildingB/Tank1/Capacity"

failed = 0


def check(name, ok, detail=""):
    global failed
    if not ok:
        failed += 1
    print("CHECK %s %s %s" % (name, "ok" if ok else "fail", detail))


def main():
    endpoint = os.environ.get("OPCXMLDA_ENDPOINT", "http://127.0.0.1:8080/")
    parsed = urlparse(endpoint)
    client = Client(host=parsed.hostname, port=parsed.port or 80)
    # The client's five-second default is generous for every operation
    # here except the long-poll below, which asks for its own timeout.
    client.timeout = 30

    # --- GetStatus ------------------------------------------------------
    status = client.getStatus()
    check(
        "status-identifies-server",
        bool(status.productVersion) and bool(status.vendorInfo),
        "productVersion=%r vendorInfo=%r" % (status.productVersion, status.vendorInfo),
    )
    check(
        "status-advertises-interface-version",
        status.supportedInterfaceVersions == "XML_DA_Version_1_0",
        repr(status.supportedInterfaceVersions),
    )
    check(
        "status-advertises-locale",
        bool(status.supportedLocaleIDs),
        repr(status.supportedLocaleIDs),
    )

    # --- Read -----------------------------------------------------------
    tags = client.read(itemList=[Tag(itemName=SPEED), Tag(itemName=TEMPERATURE), Tag(itemName=CAPACITY)])
    check("read-returns-every-item", len(tags) == 3, "got %d items" % len(tags))
    if len(tags) == 3:
        check(
            "read-preserves-request-order",
            [t.itemName for t in tags] == [SPEED, TEMPERATURE, CAPACITY],
            repr([t.itemName for t in tags]),
        )
        check(
            "read-values-are-typed-numbers",
            all(isinstance(t.value, (int, float)) and not t.error for t in tags),
            repr([(t.value, t.type, t.error) for t in tags]),
        )
        check(
            "read-reports-declared-types",
            tags[2].type in ("int", "opc:int", "xsd:int"),
            "Capacity type=%r" % tags[2].type,
        )

    # An unknown item is that item's condition, not a fault that costs
    # the client the whole request (§2.6): the good item must survive.
    mixed = client.read(itemList=[Tag(itemName=SPEED), Tag(itemName="Plant/NoSuchItem")])
    check("read-unknown-item-is-per-item", len(mixed) == 2, "got %d items" % len(mixed))
    if len(mixed) == 2:
        check(
            "read-unknown-item-keeps-good-item",
            not mixed[0].error and mixed[0].value is not None,
            repr(mixed[0]),
        )
        check(
            "read-unknown-item-reports-error",
            "E_UNKNOWNITEMNAME" in (mixed[1].error or ""),
            "error=%r" % mixed[1].error,
        )

    # --- Write ----------------------------------------------------------
    # This client spells the type attribute xsi:Type, which is a
    # different attribute in a case-sensitive language, so the only type
    # the request states is ValueTypeQualifier. Accepting it is a
    # deliberate server-side tolerance; before it, every write this
    # client issued came back E_BADTYPE.
    written = client.write(itemList=[Tag(itemName=SPEED, value=1234.0, type="double")])
    check("write-accepted", len(written) == 1 and not written[0].error, repr(written))
    if written:
        # The ordinary Write returns no value, so it has no quality to
        # report either. A <Quality> element here is read by this client
        # as the item's data type -- which is how the server's bad
        # quality on a successful write was found.
        check(
            "write-ack-carries-no-quality",
            "OPCQuality" not in (written[0].type or ""),
            "type=%r" % written[0].type,
        )

    back = client.read(itemList=[Tag(itemName=SPEED)])
    check(
        "write-round-trips",
        len(back) == 1 and abs(float(back[0].value) - 1234.0) < 1e-6,
        repr(back),
    )

    # Writable string and boolean items, so the round trip is not only
    # exercised for numbers.
    strings = client.write(itemList=[Tag(itemName=LABEL, value="interop", type="string")])
    check("write-string-accepted", len(strings) == 1 and not strings[0].error, repr(strings))
    label = client.read(itemList=[Tag(itemName=LABEL)])
    check(
        "write-string-round-trips",
        len(label) == 1 and label[0].value == "interop",
        repr(label),
    )

    bools = client.write(itemList=[Tag(itemName=RUNNING, value="true", type="boolean")])
    check("write-boolean-accepted", len(bools) == 1 and not bools[0].error, repr(bools))
    running = client.read(itemList=[Tag(itemName=RUNNING)])
    check(
        "write-boolean-round-trips",
        len(running) == 1 and running[0].value is True,
        repr(running),
    )

    # A read-only item must be refused per item, and the refusal must
    # name the condition rather than a generic failure.
    readonly = client.write(itemList=[Tag(itemName=TEMPERATURE, value=99.0, type="double")])
    check(
        "write-read-only-rejected",
        len(readonly) == 1 and "E_READONLY" in (readonly[0].error or ""),
        repr(readonly),
    )

    # Out of range: the backend clamps, and a clamp is a success code
    # carrying a caveat (S_CLAMP), not an error.
    clamped = client.write(itemList=[Tag(itemName=SPEED, value=99999.0, type="double")])
    check(
        "write-out-of-range-clamps",
        len(clamped) == 1 and "S_CLAMP" in (clamped[0].error or ""),
        repr(clamped),
    )
    after_clamp = client.read(itemList=[Tag(itemName=SPEED)])
    check(
        "write-clamp-applied-limit",
        len(after_clamp) == 1 and abs(float(after_clamp[0].value) - 3000.0) < 1e-6,
        repr(after_clamp),
    )

    # --- Browse ---------------------------------------------------------
    root = client.browse()
    check("browse-root-returns-elements", len(root) >= 1, "got %d elements" % len(root))
    plant = [e for e in root if e.name == "Plant"]
    check("browse-root-finds-plant", len(plant) == 1, repr([e.name for e in root]))
    if plant:
        check(
            "browse-branch-is-not-an-item",
            plant[0].hasChildren and not plant[0].isItem,
            "hasChildren=%s isItem=%s" % (plant[0].hasChildren, plant[0].isItem),
        )
        check(
            "browse-echoes-fully-qualified-itemname",
            plant[0].itemName == "Plant",
            "itemName=%r" % plant[0].itemName,
        )

    motor = client.browse(itemName="Plant/BuildingA/Line1/Motor1")
    leaves = [e for e in motor if e.isItem]
    check(
        "browse-leaves-are-items",
        len(leaves) == len(motor) and len(motor) >= 3,
        "%d of %d are items" % (len(leaves), len(motor)),
    )
    check(
        "browse-leaf-itemnames-are-readable",
        all(e.itemName.startswith("Plant/BuildingA/Line1/Motor1/") for e in motor),
        repr([e.itemName for e in motor]),
    )

    # --- GetProperties --------------------------------------------------
    props = client.getProperties(itemList=[Tag(itemName=SPEED)])
    check("getproperties-returns-properties", len(props) >= 1, "got %d" % len(props))
    if props:
        p = props[0]
        check(
            "getproperties-reports-datatype-and-access",
            bool(p.dataType) and bool(p.accessRights),
            "dataType=%r accessRights=%r" % (p.dataType, p.accessRights),
        )
        check(
            "getproperties-reports-writable-access",
            "readWritable" in (p.accessRights or "") or "writable" in (p.accessRights or ""),
            "accessRights=%r" % p.accessRights,
        )
        # Standard property 3 is the one complex-typed value in the
        # protocol (data type OPCQuality, §3.1.10 p.40). This client
        # reads its QualityField attribute, which the server omits when
        # the quality is good because the schema defaults it -- see
        # relax-optional-attributes.py.
        check(
            "getproperties-reports-quality",
            p.quality in ("good", "uncertain", "bad"),
            "quality=%r" % p.quality,
        )
        check(
            "getproperties-reports-scan-rate",
            bool(p.scanRate),
            "scanRate=%r" % p.scanRate,
        )
        check(
            "getproperties-reports-timestamp",
            bool(p.timestamp),
            "timestamp=%r" % p.timestamp,
        )

    # --- Subscribe / SubscriptionPolledRefresh / SubscriptionCancel -----
    sub = client.subscribe(
        itemList=[Tag(itemName=LEVEL)],
        returnValuesOnReply=True,
        subscriptionPingRate=60000,
        requestedSamplingRate=200,
        returnItemName=True,
    )
    check("subscribe-returns-handle", bool(sub.serverSubHandle), repr(sub))
    check("subscribe-reports-no-error", not sub.error, "error=%r" % sub.error)
    # The client models a Subscribe reply item as a plain dict, unlike
    # the Tag it uses everywhere else.
    check(
        "subscribe-returns-initial-values",
        len(sub.items) >= 1 and sub.items[0].get("value") is not None,
        repr(sub.items),
    )
    check(
        "subscribe-echoes-client-item-handle",
        len(sub.items) >= 1 and sub.items[0].get("clientItemHandle") == LEVEL,
        repr(sub.items),
    )

    if sub.serverSubHandle:
        # No HoldTime, so this poll must not block: "If HoldTime is
        # missing, then WaitTime is ignored" (§3.6.1 p.62). The item is
        # simulated and ticks once a second, so waiting first is what
        # puts a change in the buffer for the poll to collect -- without
        # the wait there is nothing to report and nothing to wait on,
        # which is exactly what the specification prescribes here.
        time.sleep(1.5)
        started = time.monotonic()
        # The client wants the Subscription objects here, not their
        # handles: it reads .serverSubHandle off each one itself.
        refreshed = client.subscriptionPolledRefresh(
            subscriptions=[sub],
            waitTime=2000,
            returnItemName=True,
        )
        elapsed = time.monotonic() - started
        check(
            "polled-refresh-returns-buffered-changes",
            len(refreshed) >= 1,
            "got %d subscriptions after %.2fs" % (len(refreshed), elapsed),
        )
        check(
            "polled-refresh-without-holdtime-does-not-block",
            elapsed < 1.0,
            "returned after %.2fs with WaitTime=2000 and no HoldTime" % elapsed,
        )
        if refreshed:
            # One Subscription per polled handle, each carrying its own
            # item list -- the same dict shape Subscribe replies with.
            polled = refreshed[0]
            check(
                "polled-refresh-reports-no-error",
                not polled.error,
                "error=%r" % polled.error,
            )
            check(
                "polled-refresh-values-are-usable",
                len(polled.items) >= 1 and polled.items[0].get("value") is not None,
                repr(polled.items),
            )
            # The dict key says itemName but the parser fills it from the
            # reply's ClientItemHandle attribute, which is what the
            # server echoes here. The driver subscribed with the handle
            # set to the item name, so one value proves both.
            check(
                "polled-refresh-echoes-client-item-handle",
                len(polled.items) >= 1 and polled.items[0].get("itemName") == LEVEL,
                repr(polled.items),
            )
            # The item is simulated and moves on its own, so a poll made
            # after a wait must not replay the value Subscribe returned.
            initial = sub.items[0].get("value") if sub.items else None
            check(
                "polled-refresh-value-advanced",
                initial is not None and polled.items[0].get("value") != initial,
                "initial=%r polled=%r" % (initial, polled.items[0].get("value")),
            )

        # With HoldTime set, WaitTime becomes live: the server holds
        # until HoldTime, then waits up to WaitTime for a change and
        # returns as soon as one arrives rather than sitting out the
        # full wait (§3.6.1 pp.62-63). Draining the buffer first is what
        # makes the wait real instead of being answered instantly by an
        # already-buffered change.
        client.subscriptionPolledRefresh(subscriptions=[sub], waitTime=0)
        hold = (datetime.now(timezone.utc) + timedelta(milliseconds=300)).strftime(
            "%Y-%m-%dT%H:%M:%S.%fZ"
        )
        started = time.monotonic()
        long_polled = client.subscriptionPolledRefresh(
            subscriptions=[sub],
            holdTime=hold,
            waitTime=5000,
            returnItemName=True,
        )
        elapsed = time.monotonic() - started
        check(
            "long-poll-honours-holdtime",
            elapsed >= 0.25,
            "returned after %.2fs, HoldTime was 300ms out" % elapsed,
        )
        check(
            "long-poll-returns-on-change-not-on-timeout",
            bool(long_polled) and elapsed < 4.5,
            "got %d subscriptions after %.2fs (WaitTime was 5000ms)"
            % (len(long_polled), elapsed),
        )

        cancelled = client.subscriptionCancel(serverSubHandle=sub.serverSubHandle)
        check(
            "subscription-cancel-succeeds",
            not cancelled.faultcode,
            "fault=%r" % (cancelled.faultcode,),
        )

        # Cancelling again is a no-op success, not a fault:
        # SubscriptionCancelResponse's fault list (§3.7.2 p.68) is
        # E_FAIL and E_OUTOFMEMORY only, and E_NOSUBSCRIPTION is
        # defined for SubscriptionPolledRefresh instead. See
        # docs/specification/open-questions.md OQ-9.
        again = client.subscriptionCancel(serverSubHandle=sub.serverSubHandle)
        check(
            "cancel-is-idempotent",
            not again.faultcode,
            "fault=%r detail=%r" % (again.faultcode, again.detail),
        )

        # Polling a handle that no longer exists is where the
        # specification does put the fault (§3.6, E_NOSUBSCRIPTION):
        # every polled handle was invalid, so there is no per-item list
        # to carry the condition. The server does send that fault --
        # rawrequest_test.go asserts it on the wire -- but this client's
        # refresh parser looks only for RItemList/Items/Errors and has no
        # fault branch at all, so the fault reaches it as an empty list.
        #
        # What is worth asserting through this client, then, is that it
        # gets an empty list and not a fabricated successful
        # subscription: a server that answered a dead handle with a
        # normal reply body would show up here as a non-empty result.
        stale = client.subscriptionPolledRefresh(subscriptions=[sub], waitTime=0)
        check(
            "polled-refresh-of-cancelled-handle-yields-nothing",
            stale == [],
            repr(stale),
        )

    client.close()

    if failed:
        print("%d CHECK(S) FAILED" % failed)
        return 1
    print("ALL CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
