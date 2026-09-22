import asyncio

import pytest

from deerflow.utils.async_cleanup import await_drained


@pytest.mark.parametrize("failure", [None, RuntimeError("cleanup failed"), asyncio.CancelledError("cleanup cancelled")])
async def test_repeated_cancellation_waits_for_cleanup_and_preserves_first_cancel(failure):
    entered = asyncio.Event()
    finish = asyncio.Event()
    completed = []

    async def cleanup():
        entered.set()
        await finish.wait()
        completed.append(True)
        if failure is not None:
            raise failure

    caller = asyncio.create_task(await_drained(cleanup()))
    await entered.wait()
    for reason in ("first", "second", "third"):
        caller.cancel(reason)
        await asyncio.sleep(0)
        assert not caller.done()
        assert not completed
    finish.set()
    with pytest.raises(asyncio.CancelledError, match="first") as raised:
        await caller
    assert completed == [True]
    assert raised.value.__cause__ is failure


async def test_cleanup_result_and_failure_without_cancellation():
    async def cleanup():
        return 42

    async def failed_cleanup():
        raise ValueError("close failed")

    assert await await_drained(cleanup()) == 42
    with pytest.raises(ValueError, match="close failed"):
        await await_drained(failed_cleanup())
