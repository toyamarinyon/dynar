package dynar

import "fmt"

// apiError is a DynamoDB-shaped error: a type name the SDK can decode
// into its typed exceptions plus a message.
type apiError struct {
	typ  string
	msg  string
	item map[string]any // optional, for ConditionalCheckFailedException
}

func (e *apiError) Error() string { return e.typ + ": " + e.msg }

func errValidation(format string, args ...any) *apiError {
	return &apiError{typ: "ValidationException", msg: fmt.Sprintf(format, args...)}
}

func errNotFound(msg string) *apiError {
	return &apiError{typ: "ResourceNotFoundException", msg: msg}
}

func errInUse(msg string) *apiError {
	return &apiError{typ: "ResourceInUseException", msg: msg}
}

func errConditionalCheck(msg string, item map[string]any) *apiError {
	return &apiError{typ: "ConditionalCheckFailedException", msg: msg, item: item}
}

func errInternal(err error) *apiError {
	return &apiError{typ: "InternalServerError", msg: "dynar internal error: " + err.Error()}
}
