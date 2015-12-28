package hwio

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// hwiotag represents a hwio struct tag, identified by the prefix "hwio:".
// The value of the tag is a string that is parsed as a comma-separated
// list of =-separated key-value options. If an option has no value,
// "true" is assumed to be the default value.
//
// For instance:
//
//    `hwio:"foo,bar=2,baz=false"
//
// After parsing this tag, Get("foo") will return "true", Get("bar")
// will return "2", and Get("baz") will return "false".
type hwiotag string

// parseTag extracts a hwiotag from the original reflect.StructTag as found in
// in the struct field. If the tag was not specified, an empty strtag is
// returned.
func parseTag(tag reflect.StructTag) hwiotag {
	return hwiotag(tag.Get("hwio"))
}

// Get returns the value for the specified option. If the option is not
// present in the tag, an empty string is returned. If the option is
// present but has no value, the string "true" is returned as default value.
func (t hwiotag) Get(opt string) string {
	tag := string(t)
	for tag != "" {
		var next string
		i := strings.Index(tag, ",")
		if i >= 0 {
			tag, next = tag[:i], tag[i+1:]
		}
		if tag == opt {
			return "true"
		}
		if len(tag) > len(opt) && tag[:len(opt)] == opt && tag[len(opt)] == '=' {
			val := tag[len(opt)+1:]
			i = strings.Index(val, ",")
			if i >= 0 {
				val = val[i:]
			}
			return val
		}
		tag = next
	}
	return ""
}

// MustInitRegs is like InitRegs, but panics on all errors
func MustInitRegs(data interface{}) {
	if err := InitRegs(data); err != nil {
		panic(err)
	}
}

// InitRegs initializes the IoRegs stored as fields in a data-structure, allowing
// easy configuration of values and callbacks.
//
// It parses the special "hwio" struct tag, that describes how to configure a
// register. The struct tag can have the following comma-separated options:
//
//
//    reset=0xAABB    initial (reset) value of the register. Notice that this
//                    value doesn't go through the read/write mask, so it can
//                    be used to also initialize read-only bits. If not
//                    specified, registers initialize to zero.
//
//    rwmask=0xAABB   bitmaks specifying which bits are read-write, i.e. can
//                    be written. It is common for registers to have read-only
//                    bits, so this argument allows to specify which bit can
//                    be written through bus writes. User code can of course
//                    still modify read-only bits by directly manipulating
//                    IoReg.Value. If this argument is not specified, all bits
//                    are writable.
//
//    rcb=ReadFunc    read-callback to be invoked each time the register is
//                    read. This allows to return bits whose value are computed
//                    every time the register is accessed. See IoRead32.ReadCb
//                    for more information. This option can be specified without
//                    a argument, in which case the default function name is
//                    composed by the uppercased struct field name, prefixed
//                    by "Read" (eg: for a field called "Reg1", the default
//                    read callback name is readREG1).
//
//    wcb=WriteFunc   write-callback to be invoked each time the register is
//                    written. This allows to perform operations every time the
//                    register is written. See IoWrite32.WriteCb for more
//                    information. Similar to rcb, the default argument for
//                    this option is the uppercased struct field name, prefixed
//                    by "Write".
//
func InitRegs(data interface{}) error {
	val := reflect.ValueOf(data).Elem()

	for i := 0; i < val.NumField(); i++ {
		valueField := val.Field(i)
		varField := val.Type().Field(i)
		tag := parseTag(varField.Tag)
		if tag == "" {
			continue
		}

		nbits := 0
		switch valueField.Interface().(type) {
		case Reg16:
			nbits = 16
		case Reg32:
			nbits = 32
		case Reg64:
			nbits = 64
		default:
			return fmt.Errorf("unsupported regtype: %T", valueField.Interface())
		}

		if rwmask := tag.Get("rwmask"); rwmask != "" {
			if mask, err := strconv.ParseUint(rwmask, 0, nbits); err != nil {
				return fmt.Errorf("invalid rwmask: %q", rwmask)
			} else {
				valueField.FieldByName("RoMask").SetUint(^uint64(mask))
			}
		}

		if reset := tag.Get("reset"); reset != "" {
			if rst, err := strconv.ParseUint(reset, 0, nbits); err != nil {
				return fmt.Errorf("invalid reset: %q", reset)
			} else {
				valueField.FieldByName("Value").SetUint(uint64(rst))
			}
		}

		if rcb := tag.Get("rcb"); rcb != "" {
			if rcb == "true" {
				rcb = "Read" + strings.ToUpper(varField.Name)
			}
			if meth := val.Addr().MethodByName(rcb); !meth.IsValid() {
				return fmt.Errorf("cannot find method: %q", rcb)
			} else {
				valueField.FieldByName("ReadCb").Set(meth)
			}
		}

		if wcb := tag.Get("wcb"); wcb != "" {
			if wcb == "true" {
				wcb = "Write" + strings.ToUpper(varField.Name)
			}
			if meth := val.Addr().MethodByName(wcb); !meth.IsValid() {
				return fmt.Errorf("cannot find method: %q", wcb)
			} else {
				valueField.FieldByName("WriteCb").Set(meth)
			}
		}
	}

	return nil
}

type bankRegInfo struct {
	regPtr interface{}
	offset uint32
}

// Given a structure, parse the hwid to extract the description of a bank
func bankGetRegs(data interface{}, bankNum int) ([]bankRegInfo, error) {
	val := reflect.ValueOf(data).Elem()

	var regs []bankRegInfo

	for i := 0; i < val.NumField(); i++ {
		valueField := val.Field(i)
		varField := val.Type().Field(i)
		tag := parseTag(varField.Tag)
		if tag == "" {
			continue
		}

		if offset, err := strconv.ParseInt(tag.Get("offset"), 0, 32); err != nil {
			return nil, err
		} else {
			// Check if this register declares a bank
			bank := 0
			if sbank := tag.Get("bank"); sbank != "" {
				if ibank, err := strconv.ParseUint(sbank, 0, 32); err != nil {
					return nil, err
				} else {
					bank = int(ibank)
				}
			}

			// If the bank doesn't match the requested one, skip this field
			if int(bank) != bankNum {
				continue
			}

			regs = append(regs, bankRegInfo{
				regPtr: valueField.Addr().Interface(),
				offset: uint32(offset),
			})
		}
	}

	return regs, nil
}