"""The exit gate end to end against the test BarPay (B): the real BarPay, with a fake PayPal and a
fake clock, and a fake QR reader, lock and indicator.
"""

import functools
import threading
import time
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass

from helpers import SECRET
from real_barpay import RealBarPay

from barpay_edge.barpay import Client
from barpay_edge.fakes import FakeIndicator, FakeLock, FakeQrReader
from barpay_edge.gate import REPEAT_IGNORE_NS, Gate, Timing
from barpay_edge.parts import GateState

# The PRD's time limits, apart from the pause before a report is sent again.
TIMING = Timing(report_pause=0.05)


class FakeClock:
    def __init__(self) -> None:
        self.now = 0

    def __call__(self) -> int:
        return self.now


@dataclass
class Running:
    reader: FakeQrReader
    lock: FakeLock
    indicator: FakeIndicator
    clock: FakeClock

    def scan(self, token: str) -> GateState:
        """The QR reader reads token; returns the state shown for BarPay's answer."""
        shown = len(self.indicator.shown)
        self.reader.feed(token)
        assert eventually(lambda: len(self.indicator.shown) >= shown + 2), "no answer was shown"
        return self.indicator.shown[shown + 1]


def eventually(check: Callable[[], bool], timeout: float = 10.0) -> bool:
    deadline = time.monotonic() + timeout
    while not check():
        if time.monotonic() > deadline:
            return False
        time.sleep(0.01)
    return True


@contextmanager
def gate(barpay: RealBarPay, lock: FakeLock | None = None) -> Iterator[Running]:
    r = Running(FakeQrReader(), lock or FakeLock(), FakeIndicator(), FakeClock())
    g = Gate(
        Client(barpay.url, SECRET), r.reader, r.lock, r.indicator, clock=r.clock, timing=TIMING
    )
    stop = threading.Event()
    thread = threading.Thread(target=g.run, args=(stop,), daemon=True)
    thread.start()
    try:
        yield r
    finally:
        stop.set()
        thread.join(timeout=5)


def test_a_linked_sessions_exit_token_charges_unlocks_once_and_closes_it(
    real_barpay: RealBarPay,
) -> None:
    session = real_barpay.linked_session("m_1")

    with gate(real_barpay) as g:
        shown = g.scan(real_barpay.exit_token(session))

        assert shown == GateState.THANK_YOU
        assert g.lock.unlocks == 1
        assert real_barpay.charges() == 1
        assert real_barpay.state(session) == "closed"


def test_a_scan_again_within_the_re_unlock_window_unlocks_again_without_charging(
    real_barpay: RealBarPay,
) -> None:
    session = real_barpay.linked_session("m_1")

    with gate(real_barpay) as g:
        token = real_barpay.exit_token(session)
        assert g.scan(token) == GateState.THANK_YOU
        g.clock.now = REPEAT_IGNORE_NS
        real_barpay.advance(60)

        shown = g.scan(real_barpay.exit_token(session))

        assert shown == GateState.THANK_YOU
        assert g.lock.unlocks == 2
        assert real_barpay.charges() == 1


def test_an_old_exit_token_asks_for_a_new_qr_and_the_lock_stays(
    real_barpay: RealBarPay,
) -> None:
    session = real_barpay.linked_session("m_1")
    old = real_barpay.exit_token(session)
    real_barpay.advance(real_barpay.exit_token_refresh + 1)

    with gate(real_barpay) as g:
        shown = g.scan(old)

        assert shown == GateState.SHOW_NEW_QR
        assert g.lock.unlocks == 0
        assert real_barpay.charges() == 0


def test_the_exit_token_of_a_session_not_paying_with_paypal_asks_to_pay_at_the_register(
    real_barpay: RealBarPay,
) -> None:
    # BarPay issues no exit token to a session that was never linked (UC5), so the token is one
    # of a linked session whose customer then chose the register: it no longer pays with PayPal,
    # and edge.yaml answers both alike.
    session = real_barpay.linked_session("m_1")
    token = real_barpay.exit_token(session)
    real_barpay.store("POST", f"/sessions/{session}/register-request")

    with gate(real_barpay) as g:
        shown = g.scan(token)

        assert shown == GateState.PAY_AT_REGISTER
        assert g.lock.unlocks == 0
        assert real_barpay.charges() == 0


def test_an_unlock_that_does_not_work_raises_unlock_failed_in_barpay(
    real_barpay: RealBarPay,
) -> None:
    session = real_barpay.linked_session("m_1")

    with gate(real_barpay, FakeLock(fail=True)) as g:
        shown = g.scan(real_barpay.exit_token(session))

        assert shown == GateState.UNLOCK_FAILED
        assert g.lock.unlocks == 1
        assert eventually(lambda: "unlock_failed" in real_barpay.alerts())


def test_the_exit_token_of_a_session_paid_at_the_register_unlocks(
    real_barpay: RealBarPay,
) -> None:
    session: str = real_barpay.store("POST", "/checkins", {"member_id": "m_2"}, want=201)[
        "session_id"
    ]
    real_barpay.store("POST", f"/sessions/{session}/register-request")
    real_barpay.admin(
        "POST",
        f"/sessions/{session}/register-payment",
        {"amount_recorded": {"value": 0, "currency": "USD"}},
    )

    with gate(real_barpay) as g:
        shown = g.scan(real_barpay.exit_token(session))

        assert shown == GateState.THANK_YOU
        assert g.lock.unlocks == 1
        assert real_barpay.charges() == 0


def test_staff_unlock_by_hand_while_the_gate_waits_and_it_stays_connected(
    real_barpay: RealBarPay,
) -> None:
    with gate(real_barpay) as g:
        # The gate's first wait may not have reached BarPay yet.
        assert eventually(lambda: real_barpay.unlock_gate() == 202)
        assert eventually(lambda: g.lock.unlocks == 1)
        assert g.indicator.shown[-2:] == [GateState.WORKING, GateState.UNLOCKED_BY_HAND]

        # The gate waits again at once, so staff can open it again.
        for unlocks in (2, 3):
            time.sleep(0.2)
            assert real_barpay.unlock_gate() == 202
            assert eventually(functools.partial(lambda n: g.lock.unlocks == n, unlocks))
