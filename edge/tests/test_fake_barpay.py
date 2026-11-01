"""The fake BarPay's answers are edge.yaml's, so the client's tests check it against BarPay's API
and not against something written by hand that drifted from it.
"""

import http.client
import json
import urllib.parse

import pytest
from fake_barpay import FakeBarPay, answer
from helpers import SECRET
from spec import Spec

UNLOCK_ID = "glk_A1b2C3d4E5f6G7h8"


def send(
    barpay: FakeBarPay, method: str, path: str, body: object = None, secret: str = SECRET
) -> tuple[int, bytes]:
    url = urllib.parse.urlsplit(barpay.url)
    conn = http.client.HTTPConnection(url.hostname or "", url.port, timeout=5)
    try:
        data = None if body is None else json.dumps(body).encode()
        conn.request(method, path, body=data, headers={"Authorization": "Bearer " + secret})
        resp = conn.getresponse()
        return resp.status, resp.read()
    finally:
        conn.close()


# Each answer the fake gives on its own, without a queued one, and one queued answer of each
# success edge.yaml defines.
CASES = [
    pytest.param("POST", "/v1/edge/exit-scans", {"token": "t"}, None, 200, id="scan-default"),
    pytest.param("POST", "/v1/edge/exit-scans", {"token": ""}, None, 400, id="scan-400"),
    pytest.param("POST", "/v1/edge/exit-scans", {"token": "t"}, "wrong", 401, id="scan-401"),
    pytest.param("GET", "/v1/edge/unlocks/next", None, None, 204, id="wait-204"),
    pytest.param("GET", "/v1/edge/unlocks/next", None, "wrong", 401, id="wait-401"),
    pytest.param(
        "POST",
        f"/v1/edge/unlocks/{UNLOCK_ID}/result",
        {"unlocked": True},
        None,
        404,
        id="report-404",
    ),
    pytest.param(
        "POST", "/v1/edge/unlocks/x/result", {"unlocked": True}, None, 400, id="report-400"
    ),
    pytest.param(
        "POST",
        f"/v1/edge/unlocks/{UNLOCK_ID}/result",
        {"unlocked": 1},
        None,
        400,
        id="report-400-body",
    ),
    pytest.param("POST", f"/v1/edge/unlocks/{UNLOCK_ID}/result", {}, "wrong", 401, id="report-401"),
    pytest.param("POST", "/v1/edge/signals", {"liveliness": 0.5}, None, 204, id="signals-204"),
    pytest.param("POST", "/v1/edge/signals", {"liveliness": 2}, None, 400, id="signals-400"),
    pytest.param("POST", "/v1/edge/signals", {"noise": 0.5}, None, 400, id="signals-400-field"),
    pytest.param("POST", "/v1/edge/signals", {"liveliness": 0.5}, "wrong", 401, id="signals-401"),
]


@pytest.mark.parametrize(("method", "path", "body", "secret", "status"), CASES)
def test_each_answer_the_fake_gives_is_one_edge_yaml_defines(
    barpay: FakeBarPay,
    spec: Spec,
    method: str,
    path: str,
    body: object,
    secret: str | None,
    status: int,
) -> None:
    got, data = send(barpay, method, path, body, secret or SECRET)

    assert got == status
    found = spec.find(method, path.removeprefix(spec.prefix))
    assert found is not None
    assert spec.answer_errors(found[0], got, data) == []


def test_the_queued_successes_are_ones_edge_yaml_defines(barpay: FakeBarPay, spec: Spec) -> None:
    barpay.queue(
        "scanExitToken",
        answer(200, {"unlock": True, "unlock_id": UNLOCK_ID, "reader_message": "Thank you"}),
    )
    barpay.queue("waitForUnlock", answer(200, {"unlock_id": UNLOCK_ID}))

    answers = [
        send(barpay, "POST", "/v1/edge/exit-scans", {"token": "t"}),
        send(barpay, "GET", "/v1/edge/unlocks/next"),
        send(barpay, "POST", f"/v1/edge/unlocks/{UNLOCK_ID}/result", {"unlocked": True}),
    ]

    ops = ["scanExitToken", "waitForUnlock", "reportUnlockResult"]
    for op_id, (status, data) in zip(ops, answers, strict=True):
        op = next(op for op in spec.operations if op.id == op_id)
        assert spec.answer_errors(op, status, data) == [], op_id
    assert answers[2][0] == 204


@pytest.mark.parametrize(
    ("op_id", "status", "body"),
    [
        pytest.param("scanExitToken", 200, b'{"unlock": false, "reader_message": "Hi"}', id="enum"),
        pytest.param("scanExitToken", 200, b'{"unlock": false}', id="required"),
        pytest.param("scanExitToken", 404, b"", id="status"),
        pytest.param("waitForUnlock", 200, b'{"unlock_id": "glk_x"}', id="pattern"),
        pytest.param("sendSignals", 204, b"{}", id="body-on-204"),
        pytest.param("sendSignals", 400, b'{"error": {"code": "nope", "message": "m"}}', id="code"),
    ],
)
def test_the_check_finds_an_answer_edge_yaml_does_not_define(
    spec: Spec, op_id: str, status: int, body: bytes
) -> None:
    op = next(op for op in spec.operations if op.id == op_id)

    assert spec.answer_errors(op, status, body) != []


def test_the_spec_has_the_four_operations(spec: Spec) -> None:
    assert sorted(op.id for op in spec.operations) == [
        "reportUnlockResult",
        "scanExitToken",
        "sendSignals",
        "waitForUnlock",
    ]
