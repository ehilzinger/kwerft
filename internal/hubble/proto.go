// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package hubble

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// The wire format of the relay's observer API, by hand (see the package
// comment). Field numbers after cilium/api/v1 observer.proto and flow.proto.

// encodeGetFlowsRequest encodes observer.GetFlowsRequest: follow new flows
// (3), starting at since (7), only drops and policy verdicts (whitelist 6:
// a FlowFilter with event_type 6 = EventTypeFilter{type 1}).
func encodeGetFlowsRequest(since time.Time) []byte {
	var b []byte
	b = protowire.AppendTag(b, 3, protowire.VarintType) // follow
	b = protowire.AppendVarint(b, 1)
	var filter []byte
	for _, t := range []int32{EventDrop, EventPolicyVerdict} {
		var etf []byte
		etf = protowire.AppendTag(etf, 1, protowire.VarintType) // type
		etf = protowire.AppendVarint(etf, uint64(t))
		filter = protowire.AppendTag(filter, 6, protowire.BytesType) // event_type
		filter = protowire.AppendBytes(filter, etf)
	}
	b = protowire.AppendTag(b, 6, protowire.BytesType) // whitelist
	b = protowire.AppendBytes(b, filter)
	if !since.IsZero() {
		b = protowire.AppendTag(b, 7, protowire.BytesType) // since
		b = protowire.AppendBytes(b, encodeTimestamp(since))
	}
	return b
}

func encodeTimestamp(t time.Time) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(t.Unix()))
	if n := t.Nanosecond(); n != 0 {
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(n))
	}
	return b
}

var errMalformed = errors.New("malformed protobuf message")

// fields calls fn for every field of a message; fn gets the raw value
// (varint or bytes) and returns an error to stop.
func fields(b []byte, fn func(num protowire.Number, typ protowire.Type, v uint64, bs []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return errMalformed
		}
		b = b[n:]
		var (
			v  uint64
			bs []byte
		)
		switch typ {
		case protowire.VarintType:
			v, n = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			bs, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return errMalformed
		}
		b = b[n:]
		if typ != protowire.VarintType && typ != protowire.BytesType {
			continue
		}
		if err := fn(num, typ, v, bs); err != nil {
			return err
		}
	}
	return nil
}

// response is one observer.GetFlowsResponse: a flow, or lost events.
type response struct {
	flow *Flow
	lost uint64
}

func decodeResponse(b []byte) (response, error) {
	var r response
	err := fields(b, func(num protowire.Number, typ protowire.Type, _ uint64, bs []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType: // flow
			f, err := decodeFlow(bs)
			if err != nil {
				return err
			}
			r.flow = f
		case num == 3 && typ == protowire.BytesType: // lost_events
			return fields(bs, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
				if num == 2 && typ == protowire.VarintType { // num_events_lost
					r.lost = v
				}
				return nil
			})
		}
		return nil
	})
	return r, err
}

func decodeFlow(b []byte) (*Flow, error) {
	f := &Flow{}
	var srcIP, dstIP string
	err := fields(b, func(num protowire.Number, typ protowire.Type, v uint64, bs []byte) error {
		if typ == protowire.VarintType {
			switch num {
			case 2: // verdict
				f.Verdict = Verdict(v)
			case 3: // drop_reason (deprecated, same numbers)
				if f.DropReason == 0 {
					f.DropReason = uint32(v)
				}
			case 22: // traffic_direction
				f.Direction = Direction(v)
			case 25: // drop_reason_desc
				if v != 0 {
					f.DropReason = uint32(v)
				}
			}
			return nil
		}
		switch num {
		case 1: // time
			t, err := decodeTimestamp(bs)
			f.Time = t
			return err
		case 5: // IP
			return fields(bs, func(num protowire.Number, typ protowire.Type, _ uint64, bs []byte) error {
				if typ == protowire.BytesType {
					switch num {
					case 1:
						srcIP = string(bs)
					case 2:
						dstIP = string(bs)
					}
				}
				return nil
			})
		case 6: // l4
			return decodeL4(bs, f)
		case 8: // source
			ep, err := decodeEndpoint(bs)
			f.Source = ep
			return err
		case 9: // destination
			ep, err := decodeEndpoint(bs)
			f.Destination = ep
			return err
		case 14: // destination_names
			f.DestinationNames = append(f.DestinationNames, string(bs))
		case 19: // event_type: CiliumEventType{type 1, sub_type 2}
			return fields(bs, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
				if num == 1 && typ == protowire.VarintType {
					f.EventType = int32(v)
				}
				return nil
			})
		case 26: // is_reply: google.protobuf.BoolValue{value 1}
			return fields(bs, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
				if num == 1 && typ == protowire.VarintType {
					f.Reply = v != 0
				}
				return nil
			})
		case 21001, 21002: // egress_allowed_by, ingress_allowed_by
			p, err := decodePolicy(bs)
			if err != nil {
				return err
			}
			f.AllowedBy = append(f.AllowedBy, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	f.Source.IP, f.Destination.IP = srcIP, dstIP
	return f, nil
}

func decodeTimestamp(b []byte) (time.Time, error) {
	var sec, nsec int64
	err := fields(b, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
		if typ == protowire.VarintType {
			switch num {
			case 1:
				sec = int64(v)
			case 2:
				nsec = int64(v)
			}
		}
		return nil
	})
	return time.Unix(sec, nsec).UTC(), err
}

// decodeL4 reads flow.Layer4: one of TCP 1, UDP 2, ICMPv4 3, ICMPv6 4,
// SCTP 5; TCP, UDP and SCTP carry destination_port 2.
func decodeL4(b []byte, f *Flow) error {
	return fields(b, func(num protowire.Number, typ protowire.Type, _ uint64, bs []byte) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1:
			f.Protocol = "TCP"
		case 2:
			f.Protocol = "UDP"
		case 3, 4:
			f.Protocol = "ICMP"
			return nil
		case 5:
			f.Protocol = "SCTP"
		default:
			return nil
		}
		return fields(bs, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
			if num == 2 && typ == protowire.VarintType {
				f.DstPort = uint32(v)
			}
			return nil
		})
	})
}

// decodeEndpoint reads flow.Endpoint: identity 2, namespace 3, labels 4,
// pod_name 5.
func decodeEndpoint(b []byte) (Endpoint, error) {
	var e Endpoint
	err := fields(b, func(num protowire.Number, typ protowire.Type, v uint64, bs []byte) error {
		switch {
		case num == 2 && typ == protowire.VarintType:
			e.Identity = uint32(v)
		case num == 3 && typ == protowire.BytesType:
			e.Namespace = string(bs)
		case num == 4 && typ == protowire.BytesType:
			e.Labels = append(e.Labels, string(bs))
		case num == 5 && typ == protowire.BytesType:
			e.Pod = string(bs)
		}
		return nil
	})
	return e, err
}

// decodePolicy reads flow.Policy: name 1, namespace 2.
func decodePolicy(b []byte) (Policy, error) {
	var p Policy
	err := fields(b, func(num protowire.Number, typ protowire.Type, _ uint64, bs []byte) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1:
			p.Name = string(bs)
		case 2:
			p.Namespace = string(bs)
		}
		return nil
	})
	return p, err
}

// grpcError is a non-OK gRPC status from the relay.
type grpcError struct {
	code    string
	message string
}

func (e *grpcError) Error() string {
	return fmt.Sprintf("hubble relay: grpc status %s: %s", e.code, e.message)
}
