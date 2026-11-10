These fixtures are synthetic, written by hand from anthropic-sdk-go v1.84.0
message.go, message_test.go, model.go, model_test.go, and the SDK pagination
and API-error shapes. They are not recordings and contain no live credentials.
The API version is `2023-06-01` (the SDK default `anthropic-version` header).
Future/unknown blocks and malformed or truncated inputs are deliberate robustness
cases; they do not claim to be responses currently emitted by the service.

Tests use only httptest servers. Regenerate normalized request goldens with:

    GOPROXY=off go test ./internal/llm/anthropic -update

Review request changes before accepting them. Responses are maintained by hand.
