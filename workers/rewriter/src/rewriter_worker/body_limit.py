from __future__ import annotations

import uuid
from collections.abc import Awaitable, Callable
from typing import Any

from fastapi import Request, Response
from fastapi.responses import JSONResponse
from fastapi.routing import APIRoute
from starlette.exceptions import HTTPException

BODY_TOO_LARGE_MESSAGE = "Request body exceeds the configured size limit"


class RequestBodyTooLarge(HTTPException):
    def __init__(self) -> None:
        super().__init__(status_code=413, detail=BODY_TOO_LARGE_MESSAGE)


def limited_body_route(max_body_bytes: int) -> type[APIRoute]:
    """Build a route class that rejects oversized chunked bodies while reading."""

    class LimitedBodyRoute(APIRoute):
        def get_route_handler(self) -> Callable[[Request], Awaitable[Response]]:
            original_handler = super().get_route_handler()

            async def handler(request: Request) -> Response:
                consumed = 0
                original_receive = request._receive

                async def limited_receive() -> dict[str, Any]:
                    nonlocal consumed
                    message = await original_receive()
                    if message["type"] == "http.request":
                        consumed += len(message.get("body", b""))
                        if consumed > max_body_bytes:
                            raise RequestBodyTooLarge
                    return message

                request._receive = limited_receive
                try:
                    return await original_handler(request)
                except RequestBodyTooLarge:
                    request_id = str(
                        getattr(request.state, "request_id", uuid.uuid4().hex)
                    )
                    return JSONResponse(
                        status_code=413,
                        content={
                            "error": {
                                "code": "INVALID_REQUEST",
                                "message": BODY_TOO_LARGE_MESSAGE,
                                "retryable": False,
                                "request_id": request_id,
                                "details": {},
                            }
                        },
                        headers={
                            "Cache-Control": "no-store",
                            "X-Content-Type-Options": "nosniff",
                            "X-Request-ID": request_id,
                        },
                    )

            return handler

    return LimitedBodyRoute
