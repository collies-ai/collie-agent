package session

import "encoding/json"

func jsonUnmarshalString(b []byte, s *string) error { return json.Unmarshal(b, s) }
