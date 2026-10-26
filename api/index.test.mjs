import assert from "node:assert/strict"
import { EventEmitter } from "node:events"
import { readFileSync } from "node:fs"
import test from "node:test"
import vm from "node:vm"

// 在隔离上下文中执行真实薄层源码，仅替换进程与文件系统依赖。
// 无需 Go、真实二进制、网络或真实 Key。
function loadHandler(result = { object: "list", data: [{ id: "example-paid" }] }) {
  const source = readFileSync(new URL("./index.js", import.meta.url), "utf8")
    .replace(/^import .* from "node:[^"]+"\r?\n/gm, "")
    .replace("export default async function handler", "async function handler")
  const calls = []
  let onLine
  const noop = () => {}
  const child = {
    exitCode: null,
    stdout: {},
    stderr: { on: noop },
    on: noop,
    stdin: {
      on: noop,
      write(raw) {
        const call = JSON.parse(raw)
        calls.push(call)
        queueMicrotask(() => {
          const send = (message) => onLine(JSON.stringify({ id: call.id, ...message }))
          if (call.params.stream && result.status === undefined) {
            send({ method: "sse", params: { data: "data: [DONE]\n\n" } })
            send({ method: "end" })
          } else {
            send({ result })
          }
        })
      },
    },
  }
  const context = {
    Buffer, URL, console,
    process: { env: { API_KEY: "obsolete-gateway-password" }, cwd: () => "/test" },
    spawn: () => child,
    createInterface: () => ({ on: (_event, listener) => { onLine = listener } }),
    fs: { copyFileSync: noop, chmodSync: noop },
  }
  vm.runInNewContext(`${source}\nglobalThis.handler = handler`, context)
  return { handler: context.handler, calls }
}

async function invoke(handler, path, headers = {}, body = null, method = "GET") {
  const req = new EventEmitter()
  Object.assign(req, { url: path, headers, method })
  const res = {
    status: null, headers: {}, body: "",
    writeHead(status, headers) { this.status = status; this.headers = headers },
    write(chunk) { this.body += chunk },
    end(chunk = "") { this.body += chunk },
  }
  const pending = handler(req, res)
  if (body !== null) {
    req.emit("data", Buffer.from(JSON.stringify(body)))
    req.emit("end")
  }
  await pending
  return res
}

for (const [name, headers, expected] of [
  ["Bearer", { authorization: "Bearer sk-user" }, "sk-user"],
  ["case/whitespace", { authorization: "  bEaReR\tsk-user  " }, "sk-user"],
  ["raw key", { authorization: "sk-user" }, "sk-user"],
  ["X-API-Key", { "x-api-key": "sk-user" }, "sk-user"],
  ["X-API-Key Bearer", { "x-api-key": "Bearer sk-user" }, "sk-user"],
  ["Authorization priority", { authorization: "Bearer sk-first", "x-api-key": "sk-second" }, "sk-first"],
]) {
  test(`models forwards ${name}, ignoring obsolete API_KEY`, async () => {
    const { handler, calls } = loadHandler()
    const res = await invoke(handler, "/v1/models", headers)
    assert.equal(res.status, 200)
    assert.match(res.body, /example-paid/)
    assert.equal(calls.length, 1)
    assert.equal(calls[0].params.apiKey, expected)
  })
}

for (const headers of [
  {},
  { authorization: "Bearer " },
  { authorization: "Bearer" },
  { authorization: "Basic abc" },
  { authorization: "Bearer sk-one sk-two" },
  { authorization: "sk-\x7f" },
  { authorization: " ", "x-api-key": "sk-second" },
  { authorization: ["Bearer sk-one", "Bearer sk-two"] },
]) {
  test(`invalid/missing credentials return 401: ${JSON.stringify(headers)}`, async () => {
    const { handler, calls } = loadHandler()
    for (const path of ["/v1/models", "/models", "/v1/chat/completions", "/chat/completions"]) {
      const res = await invoke(handler, path, headers)
      assert.equal(res.status, 401)
      assert.equal(JSON.parse(res.body).error.type, "authentication_error")
    }
    assert.equal(calls.length, 0)
  })
}

for (const stream of [false, true]) {
  test(`chat passes request key through RPC, stream=${stream}`, async () => {
    const { handler, calls } = loadHandler({ choices: [{ message: { content: "hello" } }] })
    const res = await invoke(handler, "/v1/chat/completions", { authorization: "Bearer sk-chat" }, {
      model: "example-paid", messages: [{ role: "user", content: "hi" }], stream,
    }, "POST")
    assert.equal(res.status, 200)
    assert.equal(calls[0].params.apiKey, "sk-chat")
    assert.equal(calls[0].params.stream, stream)
    assert.equal(calls[0].params.body.model, "example-paid")
    if (stream) assert.match(res.body, /data: \[DONE\]/)
  })
}

test("public health and preflight do not require credentials", async () => {
  const { handler, calls } = loadHandler({ status: "ok" })
  assert.equal((await invoke(handler, "/health")).status, 200)
  assert.equal((await invoke(handler, "/v1/models", {}, null, "OPTIONS")).status, 204)
  assert.equal(calls.length, 1)
  assert.equal(calls[0].method, "health")
})

for (const status of [401, 403, 429]) {
  test(`models and chat keep upstream status ${status} and Retry-After`, async () => {
    const { handler } = loadHandler({ status, retryAfter: "30", error: { error: { message: "rejected" } } })
    const headers = { authorization: "Bearer sk-user" }
    for (const path of ["/v1/models", "/v1/chat/completions"]) {
      const body = path.endsWith("models") ? null : { stream: true }
      const res = await invoke(handler, path, headers, body, body ? "POST" : "GET")
      assert.equal(res.status, status)
      assert.equal(res.headers["Retry-After"], "30")
    }
  })
}

test("concurrent requests keep independent RPC credentials", async () => {
  const { handler, calls } = loadHandler()
  const responses = await Promise.all(Array.from({ length: 24 }, (_, i) =>
    invoke(handler, "/v1/models", { authorization: `Bearer sk-user-${i}` })))
  assert.ok(responses.every((res) => res.status === 200))
  assert.equal(new Set(calls.map((call) => call.params.apiKey)).size, 24)
})
