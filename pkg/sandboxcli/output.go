package sandboxcli

import (
	"encoding/json"
	"fmt"
	"os"
)

func printJSON(v any) {
	data, _ := json.Marshal(v)
	fmt.Println(string(data))
}

func printErrorExit(msg string, exitCode int) {
	printJSON(map[string]string{"error": msg})
	os.Exit(exitCode)
}
