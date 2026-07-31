from __future__ import annotations

from omnivoice_worker.cache import VoicePromptCache


def test_weighted_lru_evicts_oldest_entry() -> None:
    cache = VoicePromptCache(max_entries=2, max_weight=10)
    cache.put("one", object(), weight=4)
    cache.put("two", object(), weight=4)

    _, hit = cache.get("one")
    assert hit

    cache.put("three", object(), weight=4)

    assert cache.get("two") == (None, False)
    assert cache.get("one")[1] is True
    assert cache.get("three")[1] is True


def test_weight_limit_can_evict_multiple_entries() -> None:
    cache = VoicePromptCache(max_entries=4, max_weight=5)
    cache.put("one", object(), weight=3)
    cache.put("two", object(), weight=3)

    assert len(cache) == 1
    assert cache.get("one") == (None, False)
