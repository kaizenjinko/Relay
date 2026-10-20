package main

import (
	"github.com/valyala/fasthttp"
)

// postJSON runs a handler once with a JSON POST to path and returns status and
// body. The context is built here so no fasthttp.Request (which embeds a
// noCopy lock) is ever copied by value.
func postJSON(h fasthttp.RequestHandler, path, body string) (int, string) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBodyString(body)
	h(ctx)
	return ctx.Response.StatusCode(), string(ctx.Response.Body())
}
