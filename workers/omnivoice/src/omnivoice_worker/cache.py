from __future__ import annotations

from collections import OrderedDict
from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True, slots=True)
class CacheEntry:
    value: Any
    weight: int


class VoicePromptCache:
    """Process-local weighted LRU.

    The weight is the bounded reference payload size. Prompt tensor size is
    backend-specific, so max_entries remains the hard GPU-memory guard.
    """

    def __init__(self, *, max_entries: int, max_weight: int) -> None:
        self._max_entries = max_entries
        self._max_weight = max_weight
        self._weight = 0
        self._entries: OrderedDict[str, CacheEntry] = OrderedDict()

    def get(self, key: str) -> tuple[Any | None, bool]:
        entry = self._entries.get(key)
        if entry is None:
            return None, False
        self._entries.move_to_end(key)
        return entry.value, True

    def put(self, key: str, value: Any, *, weight: int) -> None:
        previous = self._entries.pop(key, None)
        if previous is not None:
            self._weight -= previous.weight

        self._entries[key] = CacheEntry(value=value, weight=weight)
        self._weight += weight

        while len(self._entries) > self._max_entries or self._weight > self._max_weight:
            _, evicted = self._entries.popitem(last=False)
            self._weight -= evicted.weight

    def clear(self) -> None:
        self._entries.clear()
        self._weight = 0

    def __len__(self) -> int:
        return len(self._entries)
