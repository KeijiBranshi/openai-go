# Detailed Bug Analysis: OpenAI Go SDK Discriminated Union Deserialization Failure (Issue #483)

**Status:** Confirmed & Analyzed (Cross-referenced with OpenAI GitHub Issue #483)  
**Component:** OpenAI Go SDK (`apijson` runtime & generated `responses/response.go`)  
**Affects SDK Versions:** `openai-go/v2` (v2.0.2+), `openai-go/v3` (v3.x)  
**Severity:** High — Silently drops assistant history turns and reasoning items in multi-turn stateless requests.

---

## 1. Executive Summary

In stateless multi-turn conversations using the OpenAI Responses API, the client must explicitly pass the conversation history (including prior assistant turns and reasoning items) in the `input` parameter of the request. 

When the client sends these history items, they are structured using system-generated formats (e.g., assistant messages contain `output_text` content parts; reasoning items contain `encrypted_content`). 

However, due to a **discriminator collision** in the generated SDK and a **naive union parsing implementation** in the SDK's runtime library (`apijson`), the Go SDK silently discards these assistant turns and reasoning items during request unmarshaling. The model downstream loses all memory of prior turns, leading to redundant/repeated answers and massive token waste.

---

## 2. Root Cause Analysis

The failure occurs at the intersection of two components: the generated SDK definitions and the shared `apijson` runtime decoding logic.

### A. Discriminator Collision in `response.go`
In `responses/response.go`, the union representing request input items (`ResponseInputItemUnionParam`) is registered with `apijson` using the discriminator key `"type"`:

```go
apijson.RegisterUnion[ResponseInputItemUnionParam](
    "type",
    apijson.Discriminator[EasyInputMessageParam]("message"),           // 1. Matches "message"
    apijson.Discriminator[ResponseInputItemMessageParam]("message"),   // 2. Matches "message"
    apijson.Discriminator[ResponseOutputMessageParam]("message"),      // 3. Matches "message" (Assistant)
    ...
)
```

Three distinct Go structs all map to the **same discriminator value (`"message"`)**. This collision is correct according to the OpenAPI schema (as both user and assistant turns are technically "messages" in the API), but it triggers a fatal limitation in the decoder.

### B. Naive Struct Union Decoding in `apijson`
The `apijson` runtime (`apijson/union.go`) implements a `newStructUnionDecoder` to handle struct-based inline unions. When it encounters a discriminated union, it naively picks the **first registered variant** that matches the discriminator value and immediately returns, **never trying subsequent variants**:

```go
// In internal/apijson/union.go
return func(n gjson.Result, v reflect.Value, state *decoderState) error {
    if discriminated && n.Type == gjson.JSON && len(unionEntry.discriminatorKey) != 0 {
        discriminator := n.Get(EscapeSJSONKey(unionEntry.discriminatorKey)).Value()
        for _, decoder := range discriminatedDecoders {
            if discriminator == decoder.discriminator {
                inner := v.FieldByIndex(decoder.field.Index)
                return decoder.decoder(n, inner, state) // <--- Naive: Immediate return!
            }
        }
        return errors.New("apijson: was not able to find discriminated union variant")
    }
    ...
```

Because `EasyInputMessageParam` is registered first, **`apijson` always selects it for any JSON item with `"type": "message"`**. The correct target for assistant turns (`ResponseOutputMessageParam`) is completely shadowed and unreachable.

### C. Silent Error Swallowing in `newStructTypeDecoder`
One would expect that unmarshaling an assistant turn (which contains `output_text` parts) into `EasyInputMessageParam` (which only supports `input_text` in its content union `ResponseInputContentUnionParam`) would return an error, failing the first match and theoretically allowing a fallback.

However, `apijson`'s struct decoder (`apijson/decoder.go`) explicitly **swallows errors** encountered during the decoding of `inline` fields (which the content union is):

```go
// In internal/apijson/decoder.go (newStructTypeDecoder)
for _, inlineDecoder := range inlineDecoders {
    ...
    if !isValid {
        // If an inline decoder fails, unset the field and move on.
        if dest.IsValid() {
            dest.SetZero()
        }
        continue // <--- Silent Failure: Swallows the nested unmarshaling error
    }
}
```

Because the `output_text` parsing failure is swallowed, `EasyInputMessageParam` unmarshaling "succeeds" with a `Role` of `"assistant"` but an **empty content list (length 0)**. The actual text content of the assistant turn is silently discarded.

---

## 3. Cross-Reference with GitHub Reports

The findings above align perfectly with both reports posted on OpenAI's GitHub.

### Alignment with [Report 1 (Issue #514)](https://github.com/openai/openai-go/issues/514)
*   **Reproduction**: The failing test parses a multi-turn request where the third item is an assistant `message` with `output_text` content.
*   **Failure**: It fails on `req.Input.OfInputItemList[2].OfOutputMessage == nil` because the item was parsed into `OfMessage` (`EasyInputMessageParam`) instead of `OfOutputMessage` (`ResponseOutputMessageParam`), exactly as predicted by the discriminator collision.
*   **Workaround**: The report mentions a workaround of converting the assistant `content` from an array of `output_text` parts to a single flat `string` in the JSON:
    ```json
    "content": "Hi! How can I help you today?"
    ```
    *   **Why it works**: `EasyInputMessageParam`'s `Content` field has `OfString param.Opt[string]` as an inline option. If the client sends a flat string, `apijson` successfully unmarshals it into `OfString` without hitting the nested `ResponseInputContentUnionParam` slice unmarshaler. The text survives (though type-specific fields like `id` and `status` are still lost).

### Alignment with [Report 2 (Issue #483)](https://github.com/openai/openai-go/issues/483)
*   **Reproduction**: Marshals a `ResponseOutputMessageParam` (with `id: "msg_123"`, `status: "completed"`) to JSON, then unmarshals it back.
*   **Failure**: `OfOutputMessage` is false, and `OfMessage` is true. The restored object has lost `id` and `status` fields because they don't exist on `EasyInputMessageParam`.
*   **Suggested Solutions**:
    1.  *Use unique discriminator values*: (e.g. `output_message`). This would require changing the schema, which OpenAI might resist.
    2.  *Add secondary discriminators*: (e.g. check for `id` presence).
    3.  *Improve union decoder logic*: **(Recommended)** Enhance `newStructUnionDecoder` in `apijson` to try all matches and score them based on exactness, identical to how `newUnionDecoder` already behaves.

---

## 4. Proposed Solutions

The discriminator collision in `responses/response.go` mirrors the API schema and is generated from the OpenAPI spec, so changing the registration there would diverge from generated code. The fix should land in `internal/apijson`.

### Solution 1 — Minimal surface area: score all matches in `newStructUnionDecoder` (Recommended)

Change `internal/apijson/union.go` so that when multiple variants share a discriminator value, the decoder tries each in strict mode, scores by `exactness`, and picks the best — the same algorithm the non-discriminated branch (lines 91-133) and `newUnionDecoder` (lines 146-208) already use. Single-match cases stay fast (one strict try, behavior unchanged). Collision cases disambiguate by structural fit: `ResponseOutputMessageParam` matches the `output_text`/`id`/`status` shape exactly; `EasyInputMessageParam` falls to `loose`/`extras`.

**Scope:** ~30 lines in one file. No codegen change, no schema change, no public API change. Fixes all current and future discriminator collisions, not just `message`.

**Cost:** Each input item with a colliding discriminator pays N strict decodes instead of 1 — bounded and only when actually colliding.

**Sketch:**

```go
// internal/apijson/union.go — replacement for the discriminated branch in newStructUnionDecoder
if discriminated && n.Type == gjson.JSON && len(unionEntry.discriminatorKey) != 0 {
    discriminator := n.Get(EscapeSJSONKey(unionEntry.discriminatorKey)).Value()

    // Collect every variant whose discriminator value matches.
    matching := discriminatedDecoders[:0]
    for _, decoder := range discriminatedDecoders {
        if discriminator == decoder.discriminator {
            matching = append(matching, decoder)
        }
    }
    if len(matching) == 0 {
        return errors.New("apijson: was not able to find discriminated union variant")
    }

    // Fast path: exactly one match — preserve the existing behavior.
    if len(matching) == 1 {
        inner := v.FieldByIndex(matching[0].field.Index)
        return matching[0].decoder(n, inner, state)
    }

    // Multiple variants share this discriminator value. Try each in strict
    // mode and pick the best by exactness, mirroring the non-discriminated
    // path below and newUnionDecoder.
    bestExactness := loose - 1
    bestIdx := -1
    for i, decoder := range matching {
        sub := decoderState{strict: state.strict, exactness: exact}
        inner := v.FieldByIndex(decoder.field.Index)
        err := decoder.decoder(n, inner, &sub)
        if err != nil {
            v.FieldByIndex(decoder.field.Index).SetZero()
            continue
        }
        if sub.exactness == exact {
            bestExactness = exact
            bestIdx = i
            break
        }
        if sub.exactness > bestExactness {
            bestExactness = sub.exactness
            bestIdx = i
        }
    }
    if bestExactness < loose {
        return errors.New("apijson: was not able to coerce discriminated union variant")
    }
    // Zero out every sibling we speculatively wrote to.
    for i, decoder := range matching {
        if i != bestIdx {
            v.FieldByIndex(decoder.field.Index).SetZero()
        }
    }
    return nil
}
```

### Solution 2 — Cleanest / most modular: first-class secondary discriminators

Extend the discriminator registry so each variant carries an optional predicate alongside its primary discriminator value. `newStructUnionDecoder` evaluates predicates in registration order; the first variant whose primary value matches *and* whose secondary predicate passes wins. Predicates are pure functions over `gjson.Result`.

**Scope:** New public `apijson` API surface, codegen template update, and regeneration of `responses/response.go` registrations to emit the predicates. Two-step landing (runtime first, codegen second).

**Payoff:** Variant identity becomes explicit and testable per-variant instead of being hidden in registration order plus decoder coincidences. Codegen gets a clear, declarative place to emit disambiguators from the OpenAPI schema (e.g. `required` field sets, `oneOf` discriminator hints).

**Sketch — runtime API:**

```go
// internal/apijson/registry.go — extend the variant entry
type UnionVariant struct {
    DiscriminatorValue any
    Type               reflect.Type
    TypeFilter         gjson.Type
    Match              func(gjson.Result) bool // NEW: optional secondary predicate
}

// internal/apijson/union.go — small helpers for common predicates
func HasField(name string) func(gjson.Result) bool {
    return func(n gjson.Result) bool { return n.Get(EscapeSJSONKey(name)).Exists() }
}

func FieldIsArray(name string) func(gjson.Result) bool {
    return func(n gjson.Result) bool { return n.Get(EscapeSJSONKey(name)).IsArray() }
}

// Variant-builder overload accepted by RegisterUnion.
func Discriminator[T any](value string, opts ...func(*UnionVariant)) UnionVariant {
    v := UnionVariant{DiscriminatorValue: value, Type: reflect.TypeOf((*T)(nil)).Elem()}
    for _, opt := range opts {
        opt(&v)
    }
    return v
}

func WithMatch(fn func(gjson.Result) bool) func(*UnionVariant) {
    return func(v *UnionVariant) { v.Match = fn }
}
```

**Sketch — call site (generated):**

```go
apijson.RegisterUnion[ResponseInputItemUnionParam]("type",
    apijson.Discriminator[ResponseOutputMessageParam]("message",
        apijson.WithMatch(apijson.HasField("id")),
    ),
    apijson.Discriminator[ResponseInputItemMessageParam]("message",
        apijson.WithMatch(apijson.FieldIsArray("content")),
    ),
    apijson.Discriminator[EasyInputMessageParam]("message"), // fallback
    // ... other variants unchanged
)
```

**Sketch — decoder dispatch:**

```go
for _, decoder := range discriminatedDecoders {
    if discriminator != decoder.discriminator {
        continue
    }
    if decoder.match != nil && !decoder.match(n) {
        continue
    }
    inner := v.FieldByIndex(decoder.field.Index)
    return decoder.decoder(n, inner, state)
}
```

### Solution 3 — Stop swallowing inline errors inside a union variant

Thread a "we're inside a union variant" flag into `decoderState`, and in `newStructTypeDecoder`'s inline loop (`decoder.go:399-429`), propagate the inline error instead of zeroing the field when that flag is set. The first-match-wins union path then fails and falls through — but a try-next fallback still has to be added on top, so this ends up being Solution 1 plus extra plumbing, and risks regressing cases where inline-error-swallowing is currently load-bearing (e.g. content decoded as a flat string per the Issue #514 workaround).

**Sketch:**

```go
// internal/apijson/decoder.go — inline loop in newStructTypeDecoder
if !isValid {
    if state.inUnionVariant {
        return err // propagate so the union decoder can try the next variant
    }
    if dest.IsValid() {
        dest.SetZero()
    }
    continue
}
```

Not recommended on its own — preferred only as a hardening pass *after* Solution 1, if we discover further cases where silent inline failures mask bugs.

### Recommendation

Ship **Solution 1** now: surgical, fixes the root cause, and uses an algorithm `apijson` already implements elsewhere — so it's a consistency change, not a novel one. If discriminator collisions become a recurring schema pattern, layer **Solution 2** on top later for clarity and codegen ergonomics. The two are compatible: explicit `Match` predicates would short-circuit Solution 1's scoring loop when present.
