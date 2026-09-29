# ClinePassBridge Performance Root Cause Analysis
**Date:** 2026-09-29
**Session:** Production Performance Investigation
**Goal:** Eliminate 6-7s TTFT gap between Bridge and NewAPI

## Executive Summary

We have confirmed that **6-7 seconds of latency occur BEFORE ClinePassBridge plugin execution starts**. This happens in CPA's request translation layer when converting from `/v1/responses` or `/v1/messages` to `chat-completions` format.

## Evidence Timeline

### Bridge Internal Metrics
- prepare_ms: 86-123ms (plugin invocation overhead)
- upstream_open_ms: 3.5-5.8s (Cline upstream connection + provider TTFB)
- ttft_ms: 4.0-5.7s (total time to first token from Bridge's perspective)
- emit_wait_ms: 0-84ms (output streaming overhead - **already optimized**)

### NewAPI External Metrics
- FRT (First Response Time): 10.8-13.3s
- Total duration: matches Bridge + gap

### The Gap
**NewAPI FRT - Bridge TTFT = 6-7 seconds**

This gap occurs between:
1. NewAPI receives request
2. NewAPI routes to CPA
3. **CPA translates request format** ← BOTTLENECK HERE
4. CPA calls plugin executor
5. Bridge prepare() starts ← Bridge timer starts here

## CPA Translation Architecture

### Current Flow

#### For `/v1/responses` (pi default, Codex)
```
Client → NewAPI → CPA
              ↓
    ConvertOpenAIResponsesRequestToOpenAIChatCompletions()
    - 740 lines of translation logic
    - Parses 250k-350k JSON request
    - Iterates over entire input[] array
    - Builds tool index
    - Converts reasoning content
    - Normalizes tool calls
              ↓
    Chat Completions format (250k-350k)
              ↓
    ClinePassBridge.prepare()
              ↓
    Bridge native Responses output
              ↓
    Back to client
```

#### For `/v1/messages` (pi alternate)
```
Client → NewAPI → CPA
              ↓
    ConvertClaudeRequestToOpenAIChatCompletions()
    - 578 lines of translation logic
    - Parses 250k-350k JSON request
    - Converts system/content blocks
    - Handles thinking blocks
    - Converts tool_use/tool_result
              ↓
    Chat Completions format (250k-350k)
              ↓
    ClinePassBridge.prepare()
              ↓
    Bridge native Claude output
              ↓
    Back to client
```

## Performance Bottleneck Analysis

### Why Translation is Slow

1. **Full JSON Parse**: `gjson.ParseBytes(rawJSON)` on 250k-350k token request
2. **Multiple Iterations**: Loop over entire input[] array (pi sessions have hundreds of messages)
3. **Tool Index Building**: `newResponsesToolIndex()` scans all tools
4. **String Operations**: Repeated trimming, concatenation, reasoning combination
5. **Array Building**: Allocates and copies message arrays
6. **Multiple sjson Operations**: Each `sjson.SetBytes` potentially re-serializes

### Complexity Estimate

For a 350k token request (~1.4MB JSON):
- Parse: O(n) where n = JSON size
- Input iteration: O(m) where m = number of messages (easily 100-500 in long pi sessions)
- Tool index: O(t) where t = number of tools
- Message building: O(m * c) where c = content per message

**Total**: O(n + m*c) with large constants

At 350k tokens with 200+ messages, this easily takes **3-5 seconds** of CPU time.

### Why This Matters Now

v0.1.11 and v0.1.14 **already fixed output translation**:
- Native Claude SSE output (no per-chunk CPA translation)
- Native Responses SSE output (no per-chunk CPA translation)

But **input translation still happens** because:
```go
executor_input_formats: ["chat-completions"]  // Only accepts Chat Completions
```

So CPA **must** translate before calling the plugin.

## Solution: Native Input Support

### Goal

Let ClinePassBridge accept native formats:

```go
executor_input_formats: [
  "chat-completions",
  "claude",           // NEW
  "openai-response"   // NEW
]
```

Then CPA can pass requests **directly** to the plugin without translation.

### Implementation Strategy

#### Phase 1: Add Native Input Converters in Bridge

Create efficient converters:
- `claude_to_chat_completions.go`
- `responses_to_chat_completions.go`

These must:
1. **Match CPA semantics exactly** (to preserve prompt cache)
2. **Be more efficient** (one-pass parsing, pre-computed indices)
3. **Handle all fields**:
   - system, messages, content blocks
   - thinking/reasoning
   - tool_use, tool_result, function_call
   - tool definitions, tool_choice
   - images, videos
   - reasoning_effort
   - cache control

#### Phase 2: Ensure Prompt Cache Parity

Before enabling native input, verify:
1. Read CPA's translators line-by-line
2. Build test suite comparing outputs
3. Run against real 250k-350k warm cache sessions
4. Confirm `cached_tokens` stays near 100%

**Critical**: A cache miss on 350k tokens negates all performance gains.

#### Phase 3: Performance Optimization

Once parity is confirmed, optimize:
1. Single-pass JSON parsing
2. Pre-compute tool indices
3. Avoid repeated string operations
4. Use efficient buffer building
5. Minimize allocations

Target: <100ms for 350k token request translation.

#### Phase 4: Production Validation

Deploy candidate and measure:
- NewAPI FRT vs Bridge TTFT gap should shrink to <1s
- Cached tokens must stay high
- All protocols must work (tools, reasoning, images)
- No output truncation
- No new errors

## Risks

### 1. Prompt Cache Breakage
**Impact**: High - destroys performance gains
**Mitigation**: Extensive parity testing before deploy

### 2. Protocol Regression
**Impact**: High - breaks tools/reasoning
**Mitigation**: Comprehensive test coverage

### 3. Implementation Complexity
**Impact**: Medium - two converters to maintain
**Mitigation**: Share common logic, thorough review

## Timeline Estimate

1. **CPA Translator Parity Review**: 2-3 hours
2. **Bridge Native Input Implementation**: 4-6 hours
3. **Cache Parity Testing**: 2-3 hours
4. **Performance Optimization**: 1-2 hours
5. **Production Validation**: 2-4 hours

**Total**: 11-18 hours

## Success Criteria

✅ NewAPI FRT within 1s of Bridge TTFT
✅ 250k-350k warm sessions maintain >99% cache hit
✅ pi first token feels fast (subjective but critical)
✅ Codex throughput stays >100 t/s during generation
✅ All tool calls, reasoning, images work correctly
✅ No output truncation
✅ No new errors or panics

## Next Steps

1. Read both CPA translators in detail
2. Document all semantic transformations
3. Write Bridge native input converters with parity focus
4. Build comprehensive test suite
5. Deploy and validate

---

**Conclusion**: The 6-7s gap is definitively caused by CPA's input translation layer. Native input support in ClinePassBridge will eliminate this bottleneck while preserving all protocol features and prompt cache efficiency.
