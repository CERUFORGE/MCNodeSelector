package mcping

import "encoding/json"

// unmarshalBody 解析 SLP 应答正文；非法 JSON 由调用方转成 ErrMCBadPacket
func unmarshalBody(body []byte, v *slpJSON) error {
	return json.Unmarshal(body, v)
}
