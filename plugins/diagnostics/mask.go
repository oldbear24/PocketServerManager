package diagnostics

import (
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	optionsv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/options/v1"
)

const maskedValue = "***"

// MaskedJSON returns the JSON form of a message with every field marked with the
// `sensitive` option masked, at any depth. A payload capture only ever stores this.
func MaskedJSON(message proto.Message) string {
	clone := proto.Clone(message)
	maskMessage(clone.ProtoReflect())

	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(clone)
	if err != nil {
		return ""
	}

	return string(raw)
}

func isSensitive(field protoreflect.FieldDescriptor) bool {
	sensitive, ok := proto.GetExtension(field.Options(), optionsv1.E_Sensitive).(bool)
	return ok && sensitive
}

func maskMessage(message protoreflect.Message) {
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if isSensitive(field) {
			maskField(message, field)
			return true
		}

		switch {
		case field.IsMap():
			if field.MapValue().Message() != nil {
				value.Map().Range(func(_ protoreflect.MapKey, entry protoreflect.Value) bool {
					maskMessage(entry.Message())
					return true
				})
			}
		case field.IsList():
			if field.Message() != nil {
				list := value.List()
				for i := range list.Len() {
					maskMessage(list.Get(i).Message())
				}
			}
		case field.Message() != nil:
			maskMessage(value.Message())
		}

		return true
	})
}

// maskField replaces a sensitive value. Keys of a map stay, so that a capture still
// shows which secrets were sent; a value that cannot hold text is cleared.
func maskField(message protoreflect.Message, field protoreflect.FieldDescriptor) {
	switch {
	case field.IsMap():
		values := message.Mutable(field).Map()
		if field.MapValue().Kind() == protoreflect.StringKind {
			keys := []protoreflect.MapKey{}
			values.Range(func(key protoreflect.MapKey, _ protoreflect.Value) bool {
				keys = append(keys, key)
				return true
			})
			for _, key := range keys {
				values.Set(key, protoreflect.ValueOfString(maskedValue))
			}
			return
		}
		message.Clear(field)
	case field.IsList():
		message.Clear(field)
	case field.Kind() == protoreflect.StringKind:
		message.Set(field, protoreflect.ValueOfString(maskedValue))
	case field.Kind() == protoreflect.BytesKind:
		message.Set(field, protoreflect.ValueOfBytes([]byte(maskedValue)))
	default:
		message.Clear(field)
	}
}
