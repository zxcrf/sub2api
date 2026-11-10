#!/usr/bin/env python3
"""生成压测请求体：small（百字节级）、large（约 70KB，模拟带工具定义和多轮
历史的编码助手请求）与 xl（约 850KB，60 轮工具调用加大段工具结果的长上下文），覆盖 Anthropic Messages / OpenAI Responses / Chat 三种协议。
正文里的 __WORKER__ 由 loadgen 按 worker 替换，保证各 worker 是不同会话。"""
import json, random, sys, os

out = sys.argv[1] if len(sys.argv) > 1 else "."
random.seed(1)
words = ("the quick brown fox jumps over lazy dog function return error context "
         "request response stream token model account").split()


def text(n):
    return " ".join(random.choice(words) for _ in range(n))


def dump(name, obj):
    with open(os.path.join(out, name), "w") as f:
        json.dump(obj, f)


SID = "session __WORKER__. "
tools = [{"name": f"tool_{i}", "description": text(120),
          "input_schema": {"type": "object",
                           "properties": {f"arg{j}": {"type": "string", "description": text(20)} for j in range(5)},
                           "required": ["arg0"]}} for i in range(18)]
msgs = []
for i in range(8):
    msgs.append({"role": "user", "text": text(300)})
    msgs.append({"role": "assistant", "text": text(250)})
msgs.append({"role": "user", "text": text(200)})
msgs[0]["text"] = SID + msgs[0]["text"]
system = text(1500)

for stream in (True, False):
    sfx = "" if stream else "-nostream"
    q = SID + "Write a short poem about load testing."
    dump(f"body-small-anthropic{sfx}.json", {"model": "claude-sonnet-4-5", "max_tokens": 1024, "stream": stream,
                                             "messages": [{"role": "user", "content": q}]})
    dump(f"body-small-responses{sfx}.json", {"model": "gpt-5", "stream": stream,
                                             "input": [{"role": "user", "content": [{"type": "input_text", "text": q}]}]})
    dump(f"body-small-chat{sfx}.json", {"model": "gpt-5", "stream": stream,
                                        "messages": [{"role": "user", "content": q}]})
    dump(f"body-large-anthropic{sfx}.json", {
        "model": "claude-sonnet-4-5", "max_tokens": 8192, "stream": stream,
        "system": [{"type": "text", "text": system}], "tools": tools,
        "messages": [{"role": m["role"], "content": [{"type": "text", "text": m["text"]}]} for m in msgs]})
    dump(f"body-large-responses{sfx}.json", {
        "model": "gpt-5", "stream": stream, "instructions": system,
        "tools": [{"type": "function", "name": t["name"], "description": t["description"],
                   "parameters": t["input_schema"]} for t in tools],
        "input": [{"role": m["role"],
                   "content": [{"type": "input_text" if m["role"] == "user" else "output_text", "text": m["text"]}]}
                  for m in msgs]})
    dump(f"body-large-chat{sfx}.json", {
        "model": "gpt-5", "stream": stream,
        "messages": [{"role": "system", "content": system}] + [{"role": m["role"], "content": m["text"]} for m in msgs],
        "tools": [{"type": "function", "function": {"name": t["name"], "description": t["description"],
                                                    "parameters": t["input_schema"]}} for t in tools]})

# xl：agentic coding 长上下文，60 轮 tool_use + 大段 tool_result（只生成流式，Messages 与 Responses）
random.seed(2)
am = [{"role": "user", "content": [{"type": "text", "text": SID + text(300)}]}]
ri = [{"role": "user", "content": [{"type": "input_text", "text": SID + text(300)}]}]
for i in range(60):
    res, arg = text(2000), {"arg0": text(15)}
    am.append({"role": "assistant", "content": [
        {"type": "text", "text": text(60)},
        {"type": "tool_use", "id": f"toolu_{i:04d}", "name": "tool_1", "input": arg}]})
    am.append({"role": "user", "content": [{"type": "tool_result", "tool_use_id": f"toolu_{i:04d}", "content": res}]})
    ri.append({"type": "function_call", "call_id": f"call_{i:04d}", "name": "tool_1", "arguments": json.dumps(arg)})
    ri.append({"type": "function_call_output", "call_id": f"call_{i:04d}", "output": res})
xl_system = text(3000)
dump("body-xl-anthropic.json", {"model": "claude-sonnet-4-5", "max_tokens": 8192, "stream": True,
                                "system": [{"type": "text", "text": xl_system}], "tools": tools, "messages": am})
dump("body-xl-responses.json", {
    "model": "gpt-5", "stream": True, "instructions": xl_system,
    "tools": [{"type": "function", "name": t["name"], "description": t["description"],
               "parameters": t["input_schema"]} for t in tools],
    "input": ri})
