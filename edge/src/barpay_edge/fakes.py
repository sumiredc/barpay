"""Fake device parts, for tests and for running the edge device without its hardware.

They hold what they are given in memory and touch no device.
"""

import queue
import threading
import time
from collections.abc import Iterable, Sequence

from barpay_edge.parts import GateState


class FakeQrReader:
    def __init__(self) -> None:
        self._reads: queue.Queue[str] = queue.Queue()

    def feed(self, text: str) -> None:
        """Make text the next QR read."""
        self._reads.put(text)

    def read(self, timeout: float) -> str | None:
        try:
            return self._reads.get(timeout=timeout)
        except queue.Empty:
            return None


class FakeLock:
    """Unlocks unless set to fail or closed, counts the unlocks asked of it, and keeps whether
    it was closed.
    """

    def __init__(self, *, fail: bool = False) -> None:
        self.fail = fail
        self._unlocks = 0
        self._closed = False
        self._mu = threading.Lock()

    @property
    def unlocks(self) -> int:
        with self._mu:
            return self._unlocks

    @property
    def closed(self) -> bool:
        with self._mu:
            return self._closed

    def close(self) -> None:
        with self._mu:
            self._closed = True

    def unlock(self) -> bool:
        with self._mu:
            self._unlocks += 1
            return not self.fail and not self._closed


class FakeMicrophone:
    """Gives the blocks fed to it in order, and silence once they run out. Silence comes after
    the time a block takes to record, as from a real microphone.
    """

    def __init__(self, sample_rate: int = 16000, block_size: int = 1600) -> None:
        self._sample_rate = sample_rate
        self._block_size = block_size
        self._blocks: queue.Queue[Sequence[float]] = queue.Queue()

    @property
    def sample_rate(self) -> int:
        return self._sample_rate

    def feed(self, blocks: Iterable[Sequence[float]]) -> None:
        for block in blocks:
            self._blocks.put(block)

    def read(self) -> Sequence[float]:
        try:
            return self._blocks.get_nowait()
        except queue.Empty:
            time.sleep(self._block_size / self._sample_rate)
            return [0.0] * self._block_size


class FakeIndicator:
    """Keeps every state it was asked to show, in order, and whether it was closed."""

    def __init__(self) -> None:
        self._shown: list[GateState] = []
        self._closed = False
        self._mu = threading.Lock()

    @property
    def closed(self) -> bool:
        with self._mu:
            return self._closed

    def close(self) -> None:
        with self._mu:
            self._closed = True

    @property
    def shown(self) -> list[GateState]:
        with self._mu:
            return list(self._shown)

    def show(self, state: GateState) -> None:
        with self._mu:
            self._shown.append(state)
