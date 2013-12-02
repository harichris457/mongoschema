package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
)

var errEmptyURL = errors.New("mongoschema: no URL specificed")

func main() {
	generator := Generator{Writer: os.Stdout}
	flag.StringVar(&generator.URL, "url", "", "mongo url for dial")
	flag.StringVar(&generator.DB, "db", "", "database to use")
	flag.StringVar(&generator.Collection, "collection", "", "collection to use")
	flag.StringVar(&generator.Struct, "struct", "", "name of the struct")
	flag.StringVar(&generator.Package, "package", "", "pkg for the generate code")
	flag.BoolVar(&generator.Raw, "raw", false, "output pre-gofmt code")
	flag.Parse()

	if err := generator.Generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

type Generator struct {
	URL        string
	DB         string
	Collection string
	Package    string
	Struct     string
	Raw        bool

	Writer io.Writer
}

func (s *Generator) connect() (*mgo.Session, *mgo.Collection, error) {
	if s.URL == "" {
		return nil, nil, errEmptyURL
	}

	session, err := mgo.Dial(s.URL)
	if err != nil {
		return nil, nil, err
	}
	session.EnsureSafe(&mgo.Safe{})
	session.SetBatch(1000)
	session.SetMode(mgo.Eventual, true)
	return session, session.DB(s.DB).C(s.Collection), nil
}

func (s *Generator) Generate() error {
	session, collection, err := s.connect()
	if err != nil {
		return err
	}

	root := StructType{}
	iter := collection.Find(nil).Iter()
	m := bson.M{}
	for iter.Next(m) {
		root.Merge(NewType(m))
		m = bson.M{}
	}
	if err := iter.Close(); err != nil {
		return err
	}
	session.Close()

	const srcFmt = "package %s\ntype %s %s"
	src := fmt.Sprintf(srcFmt, s.Package, s.Struct, root.GoType())
	if s.Raw {
		fmt.Println(src)
	}
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", formatted)
	return nil
}

type Type interface {
	GoType() string
	Merge(t Type) Type
}

type LiteralType struct {
	Literal string
}

func (l LiteralType) GoType() string {
	return l.Literal
}

func (l LiteralType) Merge(t Type) Type {
	if l.GoType() == t.GoType() {
		return l
	}
	return MixedType{l, t}
}

var NilType = LiteralType{Literal: "nil"}

type MixedType []Type

func (m MixedType) GoType() string {
	var b bytes.Buffer
	fmt.Fprint(&b, "interface{} /* ")
	for i, v := range m {
		fmt.Fprint(&b, v.GoType())
		if i != len(m)-1 {
			fmt.Fprint(&b, ",")
		}
		fmt.Fprint(&b, " ")
	}
	fmt.Fprint(&b, " */")
	return b.String()
}

func (m MixedType) Merge(t Type) Type {
	for _, e := range m {
		if e.GoType() == t.GoType() {
			return m
		}
	}
	return append(m, t)
}

type PrimitiveType uint

const (
	PrimitiveBinary PrimitiveType = iota
	PrimitiveBool
	PrimitiveDouble
	PrimitiveInt32
	PrimitiveInt64
	PrimitiveObjectId
	PrimitiveString
	PrimitiveTimestamp
)

func (p PrimitiveType) GoType() string {
	switch p {
	case PrimitiveBinary:
		return "bson.Binary"
	case PrimitiveBool:
		return "bool"
	case PrimitiveDouble:
		return "float64"
	case PrimitiveInt32:
		return "int32"
	case PrimitiveInt64:
		return "int64"
	case PrimitiveString:
		return "string"
	case PrimitiveTimestamp:
		return "time.Time"
	case PrimitiveObjectId:
		return "bson.ObjectId"
	}
	panic(fmt.Sprintf("unknown primitive: %d", uint(p)))
}

func (p PrimitiveType) Merge(t Type) Type {
	if p.GoType() == t.GoType() {
		return p
	}
	return MixedType{p, t}
}

type SliceType struct {
	Type
}

func (s SliceType) GoType() string {
	return fmt.Sprintf("[]%s", s.Type.GoType())
}

func (s SliceType) Merge(t Type) Type {
	if s.GoType() == t.GoType() {
		return s
	}

	// If the target type is a slice of structs, we merge into the first struct
	// type in our own slice type.
	if targetSliceType, ok := t.(SliceType); ok {
		if targetSliceStructType, ok := targetSliceType.Type.(StructType); ok {
			// We're a slice of structs.
			if ownSliceStructType, ok := s.Type.(StructType); ok {
				s.Type = ownSliceStructType.Merge(targetSliceStructType)
				return s
			}

			// We're a slice of mixed types, one of which may or may not be a struct.
			if sliceMixedType, ok := s.Type.(MixedType); ok {
				for i, v := range sliceMixedType {
					if vStructType, ok := v.(StructType); ok {
						sliceMixedType[i] = vStructType.Merge(targetSliceStructType)
						return s
					}
				}
				return SliceType{Type: append(sliceMixedType, targetSliceStructType)}
			}
		}
	}
	return MixedType{s, t}
}

type StructType map[string]Type

func (s StructType) GoType() string {
	var buf bytes.Buffer
	fmt.Fprintln(&buf, "struct {")
	for k, v := range s {
		if isValidFieldName(k) {
			fmt.Fprintf(
				&buf,
				"%s %s `bson:\"%s,omitempty\"`\n",
				makeFieldName(k),
				v.GoType(),
				k,
			)
		} else {
			fmt.Fprintf(&buf, "/* skipping invalid field name %s */\n", k)
		}
	}
	fmt.Fprint(&buf, "}")
	return buf.String()
}

func (s StructType) Merge(t Type) Type {
	if o, ok := t.(StructType); ok {
		for k, v := range o {
			if e, ok := s[k]; ok {
				s[k] = e.Merge(v)
			} else {
				s[k] = v
			}
		}
		return s
	}
	return MixedType{s, t}
}

func NewType(v interface{}) Type {
	switch i := v.(type) {
	default:
		panic(fmt.Sprintf("cannot determine type for %v with go type %T", v, v))
	case nil:
		return NilType
	case bson.ObjectId:
		return PrimitiveObjectId
	case bson.M:
		return NewStructType(i)
	case []interface{}:
		if len(i) == 0 {
			return SliceType{Type: MixedType{}}
		}
		var s Type
		for _, v := range i {
			vt := NewType(v)
			if vt == NilType {
				continue
			}
			if s == nil {
				s = SliceType{Type: vt}
			} else {
				s.Merge(SliceType{Type: vt})
			}
		}
		if s == nil {
			return SliceType{Type: MixedType{}}
		}
		return s
	case int, int64:
		return PrimitiveInt64
	case int32:
		return PrimitiveInt32
	case bool:
		return PrimitiveBool
	case string:
		return PrimitiveString
	case time.Time, bson.MongoTimestamp:
		return PrimitiveTimestamp
	case float32, float64:
		return PrimitiveDouble
	case bson.Binary:
		return PrimitiveBinary
	}
}

func NewStructType(m bson.M) Type {
	s := StructType{}
	for k, v := range m {
		t := NewType(v)
		if t == NilType {
			continue
		}
		s[k] = t
	}
	return s
}

func isValidFieldName(n string) bool {
	if n == "" {
		return false
	}
	if strings.IndexAny(n, "!*") == -1 {
		return true
	}
	return false
}

var (
	dashUnderscoreReplacer = strings.NewReplacer("-", " ", "_", " ")
	capsRe                 = regexp.MustCompile(`([A-Z])`)
	spaceRe                = regexp.MustCompile(`(\w+)`)
	forcedUpperCase        = map[string]bool{"id": true, "url": true, "api": true}
)

func split(str string) []string {
	str = dashUnderscoreReplacer.Replace(str)
	str = capsRe.ReplaceAllString(str, " $1")
	return spaceRe.FindAllString(str, -1)
}

func makeFieldName(s string) string {
	parts := split(s)
	for i, part := range parts {
		if forcedUpperCase[strings.ToLower(part)] {
			parts[i] = strings.ToUpper(part)
		} else {
			parts[i] = strings.Title(part)
		}
	}
	camel := strings.Join(parts, "")
	runes := []rune(camel)
	for i, c := range runes {
		ok := unicode.IsLetter(c) || unicode.IsDigit(c)
		if i == 0 {
			ok = unicode.IsLetter(c)
		}
		if !ok {
			runes[i] = '_'
		}
	}
	return string(runes)
}
