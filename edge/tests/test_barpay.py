import json
import logging
import socket
import traceback
from collections.abc import Callable
from typing import Any

import pytest
from fake_barpay import FakeBarPay, answer, off_spec
from helpers import SECRET, leaks
from spec import Spec

from barpay_edge.barpay import Client, NoAnswer, ReaderMessage, Refused, ScanResult

UNLOCK_ID = "glk_A1b2C3d4E5f6G7h8"
TIMEOUT = 5.0

# One call of each operation, with a request edge.yaml accepts, and the timeout given.
CALLS: dict[str, Callable[[Client, float], object]] = {
    "scanExitToken": lambda c, t: c.scan("exit-token-0123456789", timeout=t),
    "waitForUnlock": lambda c, t: c.wait_for_unlock(timeout=t),
    "reportUnlockResult": lambda c, t: c.report_unlock(UNLOCK_ID, True, timeout=t),
    "sendSignals": lambda c, t: c.send_liveliness(0.5, timeout=t),
}


def client(barpay: FakeBarPay, secret: str = SECRET) -> Client:
    return Client(barpay.url, secret)


def unlock_answer() -> Any:
    return answer(200, {"unlock": True, "unlock_id": UNLOCK_ID, "reader_message": "Thank you"})


def closed_port_url() -> str:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    return f"http://127.0.0.1:{port}"


def error_texts(err: BaseException) -> list[str]:
    return [str(err), repr(err), "".join(traceback.format_exception(err))]


def test_every_request_is_under_v1_edge_with_the_shared_secret(spec: Spec) -> None:
    barpay = FakeBarPay(spec, SECRET, prefix="/base")
    barpay.start()
    try:
        barpay.queue("scanExitToken", unlock_answer())
        c = client(barpay)
        for call in CALLS.values():
            call(c, TIMEOUT)
    finally:
        barpay.close()

    assert len(barpay.requests) == len(CALLS)
    assert all(r.path.startswith("/base/v1/edge/") for r in barpay.requests)
    has_secret = [r.headers.get("authorization") == "Bearer " + SECRET for r in barpay.requests]
    assert all(has_secret), "a request does not carry the shared secret as a Bearer token"


def test_every_request_is_one_edge_yaml_defines(barpay: FakeBarPay, spec: Spec) -> None:
    barpay.queue("scanExitToken", unlock_answer())
    c = client(barpay)
    for call in CALLS.values():
        call(c, TIMEOUT)

    for r in barpay.requests:
        found = spec.find(r.method, r.path.removeprefix(spec.prefix))
        assert found is not None, f"{r.method} {r.path} is not in edge.yaml"
        op, params = found
        body = json.loads(r.body) if r.body else None
        assert spec.request_errors(op, params, body, bool(r.body)) == []


def test_scan_reads_an_unlock(barpay: FakeBarPay) -> None:
    barpay.queue("scanExitToken", unlock_answer())

    result = client(barpay).scan("exit-token-0123456789", timeout=TIMEOUT)

    assert result == ScanResult(True, UNLOCK_ID, ReaderMessage.THANK_YOU)


@pytest.mark.parametrize("message", list(ReaderMessage))
def test_scan_reads_each_reader_message_with_the_gate_locked(
    barpay: FakeBarPay, message: ReaderMessage
) -> None:
    barpay.queue("scanExitToken", answer(200, {"unlock": False, "reader_message": message.value}))

    result = client(barpay).scan("exit-token-0123456789", timeout=TIMEOUT)

    assert result == ScanResult(False, None, message)


def test_scan_drops_an_unlock_id_when_the_gate_stays_locked(barpay: FakeBarPay) -> None:
    barpay.queue(
        "scanExitToken",
        off_spec(
            200,
            json.dumps(
                {"unlock": False, "unlock_id": UNLOCK_ID, "reader_message": "Please see staff"}
            ).encode(),
        ),
    )

    result = client(barpay).scan("exit-token-0123456789", timeout=TIMEOUT)

    assert result.unlock_id is None


def test_scan_sends_the_token_as_read(barpay: FakeBarPay) -> None:
    client(barpay).scan("exit-token-0123456789", timeout=TIMEOUT)

    assert json.loads(barpay.requests[0].body) == {"token": "exit-token-0123456789"}


def test_wait_for_unlock_reads_an_unlock_by_hand(barpay: FakeBarPay) -> None:
    barpay.queue("waitForUnlock", answer(200, {"unlock_id": UNLOCK_ID}))

    assert client(barpay).wait_for_unlock(timeout=TIMEOUT) == UNLOCK_ID


def test_wait_for_unlock_reads_204_as_none_came(barpay: FakeBarPay) -> None:
    barpay.queue("waitForUnlock", answer(204))

    assert client(barpay).wait_for_unlock(timeout=TIMEOUT) is None


@pytest.mark.parametrize("unlocked", [True, False])
def test_report_unlock_reads_204_and_sends_the_result(barpay: FakeBarPay, unlocked: bool) -> None:
    barpay.queue("reportUnlockResult", answer(204))

    client(barpay).report_unlock(UNLOCK_ID, unlocked, timeout=TIMEOUT)

    assert barpay.requests[0].path == f"/v1/edge/unlocks/{UNLOCK_ID}/result"
    assert json.loads(barpay.requests[0].body) == {"unlocked": unlocked}


def test_send_liveliness_reads_204_and_sends_the_value(barpay: FakeBarPay) -> None:
    client(barpay).send_liveliness(0.25, timeout=TIMEOUT)

    assert json.loads(barpay.requests[0].body) == {"liveliness": 0.25}


@pytest.mark.parametrize(
    ("call", "status", "code"),
    [
        pytest.param(lambda c: c.scan("", timeout=TIMEOUT), 400, "invalid_request", id="scan-400"),
        pytest.param(
            lambda c: c.report_unlock("not-an-unlock-id", True, timeout=TIMEOUT),
            400,
            "invalid_request",
            id="report-400",
        ),
        pytest.param(
            lambda c: c.report_unlock("glk_Unknown000000000000", True, timeout=TIMEOUT),
            404,
            "unlock_not_found",
            id="report-404",
        ),
        pytest.param(
            lambda c: c.send_liveliness(1.5, timeout=TIMEOUT),
            400,
            "invalid_request",
            id="signals-400",
        ),
    ],
)
def test_an_error_edge_yaml_defines_is_refused(
    barpay: FakeBarPay, call: Callable[[Client], object], status: int, code: str
) -> None:
    with pytest.raises(Refused) as caught:
        call(client(barpay))

    assert (caught.value.status, caught.value.code) == (status, code)


@pytest.mark.parametrize("status", [400, 401, 404])
@pytest.mark.parametrize("operation", list(CALLS))
def test_an_error_status_is_refused_only_where_edge_yaml_defines_it_for_the_operation(
    barpay: FakeBarPay, spec: Spec, operation: str, status: int
) -> None:
    op = next(op for op in spec.operations if op.id == operation)
    defined = str(status) in spec.doc["paths"][op.template][op.method.lower()]["responses"]
    body = json.dumps({"error": {"code": "invalid_request", "message": "m"}}).encode()
    barpay.queue(operation, off_spec(status, body))

    with pytest.raises(Refused if defined else NoAnswer):
        CALLS[operation](client(barpay), TIMEOUT)


@pytest.mark.parametrize("operation", list(CALLS))
def test_a_wrong_shared_secret_is_refused(barpay: FakeBarPay, operation: str) -> None:
    with pytest.raises(Refused) as caught:
        CALLS[operation](client(barpay, secret="another-secret"), TIMEOUT)

    assert (caught.value.status, caught.value.code) == (401, "unauthorized")


@pytest.mark.parametrize("operation", list(CALLS))
def test_barpay_not_reached_is_no_answer(operation: str) -> None:
    with pytest.raises(NoAnswer):
        CALLS[operation](Client(closed_port_url(), SECRET), TIMEOUT)


@pytest.mark.parametrize("operation", list(CALLS))
def test_an_answer_later_than_the_timeout_is_no_answer(barpay: FakeBarPay, operation: str) -> None:
    barpay.queue(operation, off_spec(204, delay=1.0))

    with pytest.raises(NoAnswer):
        CALLS[operation](client(barpay), 0.2)


@pytest.mark.parametrize("operation", list(CALLS))
def test_a_connection_closed_without_an_answer_is_no_answer(
    barpay: FakeBarPay, operation: str
) -> None:
    barpay.queue(operation, off_spec(200, hang_up=True))

    with pytest.raises(NoAnswer):
        CALLS[operation](client(barpay), TIMEOUT)


@pytest.mark.parametrize("status", [500, 502, 503, 504])
@pytest.mark.parametrize("operation", list(CALLS))
def test_a_5xx_is_no_answer(barpay: FakeBarPay, operation: str, status: int) -> None:
    barpay.queue(
        operation, off_spec(status, json.dumps({"error": {"code": "x", "message": "y"}}).encode())
    )

    with pytest.raises(NoAnswer):
        CALLS[operation](client(barpay), TIMEOUT)


@pytest.mark.parametrize(
    ("operation", "given"),
    [
        pytest.param("scanExitToken", off_spec(200, b"<html>"), id="scan-not-json"),
        pytest.param("scanExitToken", off_spec(200, b"[]"), id="scan-not-an-object"),
        pytest.param(
            "scanExitToken", off_spec(200, b'{"unlock": false}'), id="scan-no-reader-message"
        ),
        pytest.param(
            "scanExitToken",
            off_spec(200, b'{"unlock": false, "reader_message": "Hello"}'),
            id="scan-unknown-reader-message",
        ),
        pytest.param(
            "scanExitToken",
            off_spec(200, b'{"unlock": "yes", "reader_message": "Thank you"}'),
            id="scan-unlock-not-a-boolean",
        ),
        pytest.param(
            "scanExitToken",
            off_spec(200, b'{"unlock": true, "reader_message": "Thank you"}'),
            id="scan-unlock-without-an-id",
        ),
        pytest.param(
            "scanExitToken",
            off_spec(200, b'{"unlock": true, "unlock_id": "glk_x", "reader_message": "Thank you"}'),
            id="scan-unlock-with-a-malformed-id",
        ),
        pytest.param("scanExitToken", off_spec(204), id="scan-204"),
        pytest.param("waitForUnlock", off_spec(200, b"{}"), id="wait-no-id"),
        pytest.param(
            "waitForUnlock", off_spec(200, b'{"unlock_id": "../x"}'), id="wait-malformed-id"
        ),
        pytest.param("reportUnlockResult", off_spec(200, b"{}"), id="report-200"),
        pytest.param("sendSignals", off_spec(200, b"{}"), id="signals-200"),
        pytest.param(
            "scanExitToken",
            off_spec(401, b"<html>Unauthorized</html>"),
            id="scan-401-not-an-error-body",
        ),
        pytest.param(
            "sendSignals", off_spec(400, b'{"error": "bad"}'), id="signals-400-not-an-error-body"
        ),
        pytest.param(
            "sendSignals", off_spec(307, headers={"Location": "/elsewhere"}), id="signals-redirect"
        ),
    ],
)
def test_an_answer_edge_yaml_does_not_define_is_no_answer(
    barpay: FakeBarPay, operation: str, given: Any
) -> None:
    barpay.queue(operation, given)

    with pytest.raises(NoAnswer):
        CALLS[operation](client(barpay), TIMEOUT)


def test_an_answer_larger_than_any_edge_yaml_defines_is_no_answer(barpay: FakeBarPay) -> None:
    padding = "x" * (2 << 20)
    body = {"unlock": False, "reader_message": "Thank you", "padding": padding}
    barpay.queue("scanExitToken", off_spec(200, json.dumps(body).encode()))

    with pytest.raises(NoAnswer):
        client(barpay).scan("exit-token-0123456789", timeout=TIMEOUT)


def test_a_redirect_is_not_followed(barpay: FakeBarPay, spec: Spec) -> None:
    elsewhere = FakeBarPay(spec, SECRET)
    elsewhere.start()
    try:
        barpay.queue(
            "sendSignals", off_spec(307, headers={"Location": elsewhere.url + "/v1/edge/signals"})
        )
        with pytest.raises(NoAnswer):
            client(barpay).send_liveliness(0.5, timeout=TIMEOUT)
    finally:
        elsewhere.close()

    assert elsewhere.requests == []


def test_the_secret_is_not_in_the_error_when_refused_with_401(
    barpay: FakeBarPay, spec: Spec, caplog: pytest.LogCaptureFixture
) -> None:
    other = FakeBarPay(spec, "the-secret-barpay-holds")
    other.start()
    try:
        with pytest.raises(Refused) as caught:
            Client(other.url, SECRET).scan("exit-token-0123456789", timeout=TIMEOUT)
    finally:
        other.close()
    logging.getLogger("test").exception("scan failed", exc_info=caught.value)

    assert caught.value.status == 401
    assert not leaks(*error_texts(caught.value), caplog.text)


@pytest.mark.parametrize("status", [401, 500])
def test_the_secret_is_not_in_the_error_when_barpay_quotes_it_back(
    barpay: FakeBarPay, status: int, caplog: pytest.LogCaptureFixture
) -> None:
    echoed = {"error": {"code": "unauthorized", "message": "got Authorization: Bearer " + SECRET}}
    barpay.queue("scanExitToken", off_spec(status, json.dumps(echoed).encode()))

    with pytest.raises((Refused, NoAnswer)) as caught:
        client(barpay).scan("exit-token-0123456789", timeout=TIMEOUT)
    logging.getLogger("test").exception("scan failed", exc_info=caught.value)

    assert not leaks(*error_texts(caught.value), caplog.text)


def test_the_secret_is_not_in_the_error_when_barpay_is_not_reached(
    caplog: pytest.LogCaptureFixture,
) -> None:
    with pytest.raises(NoAnswer) as caught:
        Client(closed_port_url(), SECRET).scan("exit-token-0123456789", timeout=TIMEOUT)
    logging.getLogger("test").exception("scan failed", exc_info=caught.value)

    assert not leaks(*error_texts(caught.value), caplog.text)


def test_the_secret_is_not_in_the_clients_repr() -> None:
    assert not leaks(repr(Client("http://127.0.0.1:8080", SECRET)))


@pytest.mark.parametrize(
    "secret",
    [SECRET + "\n", SECRET + " ", "シークレット" + SECRET],
    ids=["newline", "space", "non-ascii"],
)
def test_a_secret_that_cannot_be_sent_as_it_is_is_rejected_without_quoting_it(secret: str) -> None:
    with pytest.raises(ValueError, match="visible ASCII") as caught:
        Client("http://127.0.0.1:8080", secret)

    assert not leaks(*error_texts(caught.value))


@pytest.mark.parametrize(
    "url",
    ["127.0.0.1:8080", "ftp://127.0.0.1", "http://", "http://user:pw@127.0.0.1", "http://h/?q=1"],
)
def test_a_url_that_is_not_barpays_base_is_rejected(url: str) -> None:
    with pytest.raises(ValueError, match="BarPay's URL"):
        Client(url, SECRET)


@pytest.mark.parametrize("liveliness", [float("nan"), float("inf")])
def test_a_liveliness_that_is_not_a_finite_number_is_not_sent(
    barpay: FakeBarPay, liveliness: float
) -> None:
    with pytest.raises(ValueError, match="finite"):
        client(barpay).send_liveliness(liveliness, timeout=TIMEOUT)

    assert barpay.requests == []
