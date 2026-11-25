package starlink

import (
	"fmt"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Preserve reported optional fields (including explicit zero/false), rather
// than inventing measurements from protobuf getter defaults.
func reportedFields(message proto.Message) map[string]string {
	out := map[string]string{}
	var visit func(protoreflect.Message, string)
	visit = func(m protoreflect.Message, prefix string) {
		m.Range(func(fd protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			key := prefix + string(fd.Name())
			put := func(k string, v protoreflect.Value) {
				if fd.Kind() == protoreflect.MessageKind {
					visit(v.Message(), k+".")
				} else if fd.Kind() == protoreflect.EnumKind {
					enum := fd.Enum().Values().ByNumber(v.Enum())
					if enum != nil {
						out[k] = string(enum.Name())
					} else {
						out[k] = fmt.Sprint(v.Enum())
					}
				} else {
					out[k] = fmt.Sprint(v.Interface())
				}
			}
			if fd.IsList() {
				list := value.List()
				for i := 0; i < list.Len(); i++ {
					put(key+"["+strconv.Itoa(i)+"]", list.Get(i))
				}
			} else {
				put(key, value)
			}
			return true
		})
	}
	visit(message.ProtoReflect(), "")
	return out
}

func terminalDetails(t *TerminalInfo, sl *ServiceLine) map[string]string {
	out := map[string]string{
		"Service line number":       sl.Number,
		"Service line nickname":     sl.Nickname,
		"Location nickname":         t.LocationNickname,
		"Kit serial":                t.SerialNumber,
		"Dish serial":               t.DishSerialNumber,
		"Active service (roster)":   fmt.Sprint(t.Active),
		"Offline (roster snapshot)": fmt.Sprint(t.IsOffline),
	}
	if (t.Present != nil && t.Present["lat"] && t.Present["lon"]) || (t.Present == nil && (t.Lat != 0 || t.Lon != 0)) {
		out["Latitude / longitude (roster)"] = fmt.Sprintf("%.6f, %.6f", t.Lat, t.Lon)
	}
	if t.Present != nil && !t.Present["active"] {
		delete(out, "Active service (roster)")
	}
	if t.Present != nil && !t.Present["isOffline"] {
		delete(out, "Offline (roster snapshot)")
	}
	for label, timestamp := range map[string]time.Time{
		"Last connected (roster, UTC)":    t.LastConnected,
		"Last disconnected (roster, UTC)": t.LastDisconnected,
	} {
		if !timestamp.IsZero() {
			out[label] = timestamp.UTC().Format(time.RFC3339)
		}

	}
	return out
}

func routerDetails(r *RouterInfo, sl *ServiceLine) map[string]string {
	out := map[string]string{
		"Service line number":       sl.Number,
		"Service line nickname":     sl.Nickname,
		"Hardware version (roster)": r.HardwareVersion,
	}
	if r.Present == nil || r.Present["isBypassed"] {
		out["Bypassed (roster)"] = fmt.Sprint(r.IsBypassed)
	}
	return out
}
