"""The edge device's BarPay client against the test BarPay: the real BarPay, with a fake PayPal and
a fake clock.
"""

import contextlib
import os
from pathlib import Path

import pytest
from helpers import SECRET
from real_barpay import RealBarPay, start

from barpay_edge.barpay import Client, ReaderMessage, ScanResult

TIMEOUT = 10.0


def client(barpay: RealBarPay) -> Client:
    return Client(barpay.url, SECRET)


def test_a_linked_sessions_exit_token_unlocks_and_the_result_is_taken(
    real_barpay: RealBarPay,
) -> None:
    session = real_barpay.linked_session("m_1")
    c = client(real_barpay)

    result = c.scan(real_barpay.exit_token(session), timeout=TIMEOUT)

    assert result.unlock
    assert result.reader_message == ReaderMessage.THANK_YOU
    assert result.unlock_id is not None
    # Raises unless BarPay answers 204.
    c.report_unlock(result.unlock_id, True, timeout=TIMEOUT)


def test_the_previous_exit_token_asks_for_a_new_qr_once_the_refresh_has_passed(
    real_barpay: RealBarPay,
) -> None:
    session = real_barpay.linked_session("m_1")
    previous = real_barpay.exit_token(session)

    real_barpay.advance(real_barpay.exit_token_refresh + 1)

    # A bool, so a failing assertion does not print the exit tokens.
    changed = real_barpay.exit_token(session) != previous
    assert changed, "the exit token did not change after the refresh"
    result = client(real_barpay).scan(previous, timeout=TIMEOUT)
    assert result == ScanResult(False, None, ReaderMessage.SHOW_NEW_QR)


def test_the_charge_of_an_unlocking_scan_is_counted_once(real_barpay: RealBarPay) -> None:
    session = real_barpay.linked_session("m_1")
    assert real_barpay.charges() == 0

    result = client(real_barpay).scan(real_barpay.exit_token(session), timeout=TIMEOUT)

    assert result.unlock
    assert real_barpay.charges() == 1


def test_the_test_barpay_is_gone_once_the_test_ends(
    real_barpay_binary: Path, tmp_path: Path
) -> None:
    with start(real_barpay_binary, tmp_path) as barpay:
        pid = barpay.pid
        os.kill(pid, 0)  # running
        # Its store is under the test's tmp_path, so a killed one leaves nothing elsewhere.
        assert list(tmp_path.glob("barpay-test-*"))

    with pytest.raises(ProcessLookupError):
        os.kill(pid, 0)


class _TestFailed(Exception):
    pass


def test_the_test_barpay_is_gone_even_when_the_test_fails(
    real_barpay_binary: Path, tmp_path: Path
) -> None:
    pid = 0
    with contextlib.suppress(_TestFailed), start(real_barpay_binary, tmp_path) as barpay:
        pid = barpay.pid
        raise _TestFailed

    with pytest.raises(ProcessLookupError):
        os.kill(pid, 0)
