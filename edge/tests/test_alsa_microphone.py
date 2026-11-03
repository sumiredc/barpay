import struct
import sys
import types
from typing import Any

import pytest

from barpay_edge.alsa_microphone import AlsaMicrophone
from barpay_edge.parts import MicrophoneError

S16_LE, S32_LE = 2, 10


class _ALSAAudioError(Exception):
    pass


class _FakePCM:
    """Gives what the fake device gives, as pyalsaaudio's PCM does."""

    def __init__(self, device_gives: dict[str, Any], reads: list[Any], **kwargs: Any) -> None:
        self.kwargs = kwargs
        self.closed = False
        self._gives = device_gives
        self._reads = reads

    def info(self) -> dict[str, Any]:
        return dict(self._gives)

    def read(self) -> tuple[int, bytes]:
        step = self._reads.pop(0)
        if isinstance(step, Exception):
            raise step
        result: tuple[int, bytes] = step
        return result

    def close(self) -> None:
        self.closed = True


class FakeAlsa:
    """Stands in for the alsaaudio module in this file's tests only; every other test still
    finds alsaaudio blocked (conftest.py).
    """

    def __init__(self, gives: dict[str, Any], reads: list[Any]) -> None:
        self.gives = gives
        self.reads = reads
        self.opened: list[_FakePCM] = []
        self.module = types.ModuleType("alsaaudio")
        for name, value in {
            "PCM_CAPTURE": 1,
            "PCM_NORMAL": 0,
            "PCM_FORMAT_S16_LE": S16_LE,
            "ALSAAudioError": _ALSAAudioError,
            "PCM": self._pcm,
        }.items():
            setattr(self.module, name, value)

    def _pcm(self, **kwargs: Any) -> _FakePCM:
        pcm = _FakePCM(self.gives, self.reads, **kwargs)
        self.opened.append(pcm)
        return pcm


ONE_S16_CHANNEL = {"format": S16_LE, "format_name": "S16_LE", "channels": 1}
BLOCK = (2, struct.pack("<2h", 16384, -16384))


@pytest.fixture
def alsa(monkeypatch: pytest.MonkeyPatch) -> FakeAlsa:
    fake = FakeAlsa(dict(ONE_S16_CHANNEL), [])
    monkeypatch.setitem(sys.modules, "alsaaudio", fake.module)
    return fake


def test_it_reads_one_channel_of_16_bit_samples(alsa: FakeAlsa) -> None:
    alsa.reads.append(BLOCK)

    samples = AlsaMicrophone("plughw:Loopback,1,0").read()

    assert list(samples) == [0.5, -0.5]
    (pcm,) = alsa.opened
    assert pcm.kwargs["device"] == "plughw:Loopback,1,0"
    assert (pcm.kwargs["channels"], pcm.kwargs["format"]) == (1, S16_LE)


@pytest.mark.parametrize(
    "gives",
    [
        pytest.param({"format": S32_LE, "format_name": "S32_LE", "channels": 1}, id="format"),
        pytest.param({"format": S16_LE, "format_name": "S16_LE", "channels": 2}, id="channels"),
    ],
)
def test_a_device_that_gives_other_samples_is_not_read(
    alsa: FakeAlsa, gives: dict[str, Any]
) -> None:
    alsa.gives.update(gives)
    alsa.reads.append(BLOCK)
    mic = AlsaMicrophone("hw:1,0")

    with pytest.raises(MicrophoneError, match=r"'hw:1,0' gives .*plughw"):
        mic.read()

    assert alsa.opened[0].closed
    assert alsa.reads == [BLOCK], "a block was read from a device giving other samples"


def test_a_device_that_gives_other_samples_is_tried_again_at_the_next_read(alsa: FakeAlsa) -> None:
    alsa.gives["channels"] = 2
    mic = AlsaMicrophone("hw:1,0")
    with pytest.raises(MicrophoneError):
        mic.read()

    alsa.gives["channels"] = 1
    alsa.reads.append(BLOCK)

    assert list(mic.read()) == [0.5, -0.5]
    assert len(alsa.opened) == 2


def test_a_failed_read_closes_the_device_and_the_next_read_opens_it_again(alsa: FakeAlsa) -> None:
    alsa.reads.extend([_ALSAAudioError("No such device"), BLOCK])
    mic = AlsaMicrophone("default")

    with pytest.raises(MicrophoneError, match="No such device"):
        mic.read()
    assert list(mic.read()) == [0.5, -0.5]

    assert alsa.opened[0].closed
    assert len(alsa.opened) == 2


def test_an_overrun_gives_an_empty_block(alsa: FakeAlsa) -> None:
    alsa.reads.append((-32, b""))

    assert list(AlsaMicrophone("default").read()) == []
