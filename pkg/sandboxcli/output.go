package sandboxcli

import (
	"encoding/json"
	"fmt"
)

// ExitError is an error that carries an exit code.
// Callers at the top level (main) check for this type to exit with the proper code.
type ExitError struct {
	Message  string
	ExitCode int
}

func (e *ExitError) Error() string {
	return e.Message
}

func printJSON(v any) {
	data, _ := json.Marshal(v)
	fmt.Println(string(data))
}

// printErrorExit prints the error as JSON and returns an ExitError with the given exit code.
// Unlike os.Exit, returning the error allows deferred cleanup (e.g. client.Close()) to run.
func printErrorExit(msg string, exitCode int) *ExitError {
	printJSON(map[string]string{"error": msg})
	return &ExitError{Message: msg, ExitCode: exitCode}
}
