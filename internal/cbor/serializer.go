/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The decoder in this file is adapted from k8s.io/apimachinery/pkg/runtime/serializer/cbor
// and k8s.io/client-go/dynamic v0.32: the decoder options are the same, the size limits
// are those of v0.36, and lists are filled as UnstructuredJSONScheme fills them.

package cbor

import (
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"

	fxcbor "github.com/fxamacker/cbor/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apicbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor"
)

// decodeMode has the options of the apimachinery CBOR decoder in its lax
// mode, which ignores unknown fields, with the size limits of v0.36. Before
// v0.36 apimachinery rejects arrays and maps with more than 1024 elements and
// nesting deeper than 64 levels.
var decodeMode = func() fxcbor.DecMode {
	mode, err := fxcbor.DecOptions{
		DupMapKey:                fxcbor.DupMapKeyEnforcedAPF,
		TimeTag:                  fxcbor.DecTagOptional,
		MaxNestedLevels:          10000,
		MaxArrayElements:         2147483647,
		MaxMapPairs:              2097152,
		IndefLength:              fxcbor.IndefLengthAllowed,
		TagsMd:                   fxcbor.TagsAllowed,
		IntDec:                   fxcbor.IntDecConvertSignedOrFail,
		MapKeyByteString:         fxcbor.MapKeyByteStringForbidden,
		DefaultMapType:           reflect.TypeFor[map[string]any](),
		UTF8:                     fxcbor.UTF8RejectInvalid,
		FieldNameMatching:        fxcbor.FieldNameMatchingCaseSensitive,
		DefaultByteStringType:    reflect.TypeFor[string](),
		ByteStringToString:       fxcbor.ByteStringToStringAllowedWithExpectedLaterEncoding,
		FieldNameByteString:      fxcbor.FieldNameByteStringAllowed,
		UnrecognizedTagToAny:     fxcbor.UnrecognizedTagContentToAny,
		TimeTagToAny:             fxcbor.TimeTagToRFC3339Nano,
		ByteStringToTime:         fxcbor.ByteStringToTimeAllowed,
		NaN:                      fxcbor.NaNDecodeForbidden,
		Inf:                      fxcbor.InfDecodeForbidden,
		ByteStringExpectedFormat: fxcbor.ByteStringExpectedBase64,
		BignumTag:                fxcbor.BignumTagForbidden,
		SimpleValues:             simpleValues(),
		BinaryUnmarshaler:        fxcbor.BinaryUnmarshalerNone,
	}.DecMode()
	if err != nil {
		panic(err)
	}

	return mode
}()

// simpleValues accepts only the simple values false, true and null.
func simpleValues() *fxcbor.SimpleValueRegistry {
	opts := make([]func(*fxcbor.SimpleValueRegistry) error, 0, 256)

	for sv := 0; sv <= 255; sv++ {
		switch sv {
		case 20, 21, 22: // false, true, null
		case 24, 25, 26, 27, 28, 29, 30, 31: // reserved, rejected by the library itself
		default:
			opts = append(opts, fxcbor.WithRejectedSimpleValue(fxcbor.SimpleValue(sv)))
		}
	}

	registry, err := fxcbor.NewSimpleValueRegistryFromDefaults(opts...)
	if err != nil {
		panic(err)
	}

	return registry
}

// serializer decodes CBOR with decodeMode. Encoding is left to apimachinery,
// whose encoder has no size limits.
type serializer struct {
	runtime.Encoder

	creator runtime.ObjectCreater
	typer   runtime.ObjectTyper
}

func newSerializer(creator runtime.ObjectCreater, typer runtime.ObjectTyper) *serializer {
	return &serializer{
		Encoder: apicbor.NewSerializer(creator, typer),
		creator: creator,
		typer:   typer,
	}
}

func (s *serializer) Decode(data []byte, gvk *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	// Decoding into Unstructured needs no preliminary pass for the kind.
	if u, ok := into.(runtime.Unstructured); ok {
		if err := decodeUnstructured(data, u); err != nil {
			actual, interpretErr := interpret(data)
			if interpretErr != nil {
				return nil, nil, interpretErr
			}

			if gvk != nil {
				*actual = gvkWithDefaults(*actual, *gvk)
			}

			return nil, actual, err
		}

		actual := into.GetObjectKind().GroupVersionKind()
		if err := checkKind(actual, data); err != nil {
			return nil, &actual, err
		}

		return into, &actual, nil
	}

	actual, err := interpret(data)
	if err != nil {
		return nil, nil, err
	}

	if gvk != nil {
		*actual = gvkWithDefaults(*actual, *gvk)
	}

	if into != nil {
		types, _, err := s.typer.ObjectKinds(into)
		if err != nil {
			return nil, actual, err
		}

		*actual = gvkWithDefaults(*actual, types[0])
	}

	if err := checkKind(*actual, data); err != nil {
		return nil, actual, err
	}

	obj, err := runtime.UseOrCreateObject(s.typer, s.creator, *actual, into)
	if err != nil {
		return nil, actual, err
	}

	if u, ok := obj.(runtime.Unstructured); ok {
		err = decodeUnstructured(data, u)
	} else {
		err = decodeMode.Unmarshal(data, obj)
	}

	if err != nil {
		return nil, actual, err
	}

	return obj, actual, nil
}

// decodeUnstructured fills u with the same content UnstructuredJSONScheme
// would decode from the JSON form of data.
func decodeUnstructured(data []byte, u runtime.Unstructured) error {
	var content map[string]any
	if err := decodeMode.Unmarshal(data, &content); err != nil {
		return err
	}

	if list, ok := u.(*unstructured.UnstructuredList); ok {
		return fillList(list, content)
	}

	u.SetUnstructuredContent(content)

	return nil
}

// fillList fills list as UnstructuredJSONScheme does. SetUnstructuredContent
// would keep "items" in Object and leave the items of typed lists, such as
// PodList, without a kind.
func fillList(list *unstructured.UnstructuredList, content map[string]any) error {
	var items []any

	switch v := content["items"].(type) {
	case nil:
	case []any:
		items = v
	default:
		return errors.New("items field of UnstructuredList must be an array or null")
	}

	delete(content, "items")
	list.Object = content

	apiVersion := list.GetAPIVersion()
	kind := strings.TrimSuffix(list.GetKind(), "List")

	list.Items = make([]unstructured.Unstructured, 0, len(items))

	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return errors.New("elements of the items field of UnstructuredList must be maps")
		}

		u := unstructured.Unstructured{Object: object}
		if u.GetKind() == "" && u.GetAPIVersion() == "" {
			u.SetKind(kind)
			u.SetAPIVersion(apiVersion)
		}

		list.Items = append(list.Items, u)
	}

	return nil
}

// interpret reads the group, version and kind of an encoded object.
func interpret(data []byte) (*schema.GroupVersionKind, error) {
	var tm metav1.TypeMeta
	if err := decodeMode.Unmarshal(data, &tm); err != nil {
		return nil, fmt.Errorf("unable to determine group/version/kind: %w", err)
	}

	actual := tm.GroupVersionKind()

	return &actual, nil
}

func checkKind(gvk schema.GroupVersionKind, data []byte) error {
	if gvk.Kind == "" {
		return runtime.NewMissingKindErr(diagnose(data))
	}

	if gvk.Version == "" {
		return runtime.NewMissingVersionErr(diagnose(data))
	}

	return nil
}

func gvkWithDefaults(actual, defaultGVK schema.GroupVersionKind) schema.GroupVersionKind {
	if actual.Kind == "" {
		actual.Kind = defaultGVK.Kind
	}

	if actual.Version == "" && actual.Group == "" {
		actual.Group = defaultGVK.Group
		actual.Version = defaultGVK.Version
	}

	if actual.Version == "" && actual.Group == defaultGVK.Group {
		actual.Version = defaultGVK.Version
	}

	return actual
}

func diagnose(data []byte) string {
	if diag, err := fxcbor.Diagnose(data); err == nil {
		return diag
	}

	return hex.EncodeToString(data)
}

// scheme has the meta/v1 types, such as Status and WatchEvent, that the
// dynamic client decodes into typed objects.
var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	metav1.AddToGroupVersion(s, schema.GroupVersion{Version: "v1"})

	return s
}()

// unstructuredCreater and unstructuredTyper are those of the dynamic client:
// meta/v1 types are typed, everything else is Unstructured.
type unstructuredCreater struct{}

func (unstructuredCreater) New(kind schema.GroupVersionKind) (runtime.Object, error) {
	if obj, err := scheme.New(kind); err == nil {
		return obj, nil
	}

	obj := &unstructured.Unstructured{}
	obj.GetObjectKind().SetGroupVersionKind(kind)

	return obj, nil
}

type unstructuredTyper struct{}

func (unstructuredTyper) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	kinds, unversioned, err := scheme.ObjectKinds(obj)
	if err == nil {
		return kinds, unversioned, nil
	}

	if _, ok := obj.(runtime.Unstructured); ok && !obj.GetObjectKind().GroupVersionKind().Empty() {
		return []schema.GroupVersionKind{obj.GetObjectKind().GroupVersionKind()}, false, nil
	}

	return nil, false, err
}

func (unstructuredTyper) Recognizes(schema.GroupVersionKind) bool {
	return true
}

// negotiatedSerializer adds CBOR to the media types of the dynamic client.
// Watch responses come as a CBOR sequence, which client-go maps to the CBOR
// serializer only while its feature gate is on, so the sequence is listed too.
type negotiatedSerializer struct {
	runtime.NegotiatedSerializer

	mediaTypes []runtime.SerializerInfo
}

func newNegotiatedSerializer(base runtime.NegotiatedSerializer) negotiatedSerializer {
	object := newSerializer(unstructuredCreater{}, unstructuredTyper{})

	mediaTypes := make([]runtime.SerializerInfo, 0, len(base.SupportedMediaTypes())+2)

	for _, info := range base.SupportedMediaTypes() {
		// With the client-go feature gate on, the base has the CBOR
		// serializer of apimachinery with its size limits: replace it.
		if info.MediaType != runtime.ContentTypeCBOR && info.MediaType != runtime.ContentTypeCBORSequence {
			mediaTypes = append(mediaTypes, info)
		}
	}

	mediaTypes = append(mediaTypes,
		runtime.SerializerInfo{
			MediaType:        runtime.ContentTypeCBOR,
			MediaTypeType:    "application",
			MediaTypeSubType: "cbor",
			Serializer:       object,
		},
		runtime.SerializerInfo{
			MediaType:        runtime.ContentTypeCBORSequence,
			MediaTypeType:    "application",
			MediaTypeSubType: "cbor-seq",
			Serializer:       object,
			StreamSerializer: &runtime.StreamSerializerInfo{
				Serializer: newSerializer(scheme, scheme),
				Framer:     apicbor.NewFramer(),
			},
		},
	)

	return negotiatedSerializer{NegotiatedSerializer: base, mediaTypes: mediaTypes}
}

func (s negotiatedSerializer) SupportedMediaTypes() []runtime.SerializerInfo {
	return s.mediaTypes
}
