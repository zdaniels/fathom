package agent

import "encoding/json"

// jsonMarshal is a thin wrapper to keep encoding/json out of the agent loop
// import group — keeps the file's import block tidy.
func jsonMarshal(v interface{}) ([]byte, error) { return json.Marshal(v) }
