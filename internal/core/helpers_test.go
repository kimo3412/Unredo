package core

import "encoding/json"

func jsonMarshalString(s string) ([]byte, error) { return json.Marshal(s) }
