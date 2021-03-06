package netlink

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"syscall"

	"github.com/coreos/fleet/Godeps/_workspace/src/github.com/vishvananda/netlink/nl"
)

// FilterDel will delete a filter from the system.
// Equivalent to: `tc filter del $filter`
func FilterDel(filter Filter) error {
	req := nl.NewNetlinkRequest(syscall.RTM_DELTFILTER, syscall.NLM_F_ACK)
	base := filter.Attrs()
	msg := &nl.TcMsg{
		Family:  nl.FAMILY_ALL,
		Ifindex: int32(base.LinkIndex),
		Handle:  base.Handle,
		Parent:  base.Parent,
		Info:    MakeHandle(base.Priority, nl.Swap16(base.Protocol)),
	}
	req.AddData(msg)

	_, err := req.Execute(syscall.NETLINK_ROUTE, 0)
	return err
}

// FilterAdd will add a filter to the system.
// Equivalent to: `tc filter add $filter`
func FilterAdd(filter Filter) error {
	native = nl.NativeEndian()
	req := nl.NewNetlinkRequest(syscall.RTM_NEWTFILTER, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL|syscall.NLM_F_ACK)
	base := filter.Attrs()
	msg := &nl.TcMsg{
		Family:  nl.FAMILY_ALL,
		Ifindex: int32(base.LinkIndex),
		Handle:  base.Handle,
		Parent:  base.Parent,
		Info:    MakeHandle(base.Priority, nl.Swap16(base.Protocol)),
	}
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(nl.TCA_KIND, nl.ZeroTerminated(filter.Type())))

	options := nl.NewRtAttr(nl.TCA_OPTIONS, nil)
	if u32, ok := filter.(*U32); ok {
		// match all
		sel := nl.TcU32Sel{
			Nkeys: 1,
			Flags: nl.TC_U32_TERMINAL,
		}
		sel.Keys = append(sel.Keys, nl.TcU32Key{})
		nl.NewRtAttrChild(options, nl.TCA_U32_SEL, sel.Serialize())
		if u32.ClassId != 0 {
			nl.NewRtAttrChild(options, nl.TCA_U32_CLASSID, nl.Uint32Attr(u32.ClassId))
		}
		actionsAttr := nl.NewRtAttrChild(options, nl.TCA_U32_ACT, nil)
		// backwards compatibility
		if u32.RedirIndex != 0 {
			u32.Actions = append([]Action{NewMirredAction(u32.RedirIndex)}, u32.Actions...)
		}
		if err := encodeActions(actionsAttr, u32.Actions); err != nil {
			return err
		}
	} else if fw, ok := filter.(*Fw); ok {
		if fw.Mask != 0 {
			b := make([]byte, 4)
			native.PutUint32(b, fw.Mask)
			nl.NewRtAttrChild(options, nl.TCA_FW_MASK, b)
		}
		if fw.InDev != "" {
			nl.NewRtAttrChild(options, nl.TCA_FW_INDEV, nl.ZeroTerminated(fw.InDev))
		}
		if (fw.Police != nl.TcPolice{}) {

			police := nl.NewRtAttrChild(options, nl.TCA_FW_POLICE, nil)
			nl.NewRtAttrChild(police, nl.TCA_POLICE_TBF, fw.Police.Serialize())
			if (fw.Police.Rate != nl.TcRateSpec{}) {
				payload := SerializeRtab(fw.Rtab)
				nl.NewRtAttrChild(police, nl.TCA_POLICE_RATE, payload)
			}
			if (fw.Police.PeakRate != nl.TcRateSpec{}) {
				payload := SerializeRtab(fw.Ptab)
				nl.NewRtAttrChild(police, nl.TCA_POLICE_PEAKRATE, payload)
			}
		}
		if fw.ClassId != 0 {
			b := make([]byte, 4)
			native.PutUint32(b, fw.ClassId)
			nl.NewRtAttrChild(options, nl.TCA_FW_CLASSID, b)
		}
	} else if bpf, ok := filter.(*BpfFilter); ok {
		var bpf_flags uint32
		if bpf.ClassId != 0 {
			nl.NewRtAttrChild(options, nl.TCA_BPF_CLASSID, nl.Uint32Attr(bpf.ClassId))
		}
		if bpf.Fd >= 0 {
			nl.NewRtAttrChild(options, nl.TCA_BPF_FD, nl.Uint32Attr((uint32(bpf.Fd))))
		}
		if bpf.Name != "" {
			nl.NewRtAttrChild(options, nl.TCA_BPF_NAME, nl.ZeroTerminated(bpf.Name))
		}
		if bpf.DirectAction {
			bpf_flags |= nl.TCA_BPF_FLAG_ACT_DIRECT
		}
		nl.NewRtAttrChild(options, nl.TCA_BPF_FLAGS, nl.Uint32Attr(bpf_flags))
	}

	req.AddData(options)
	_, err := req.Execute(syscall.NETLINK_ROUTE, 0)
	return err
}

// FilterList gets a list of filters in the system.
// Equivalent to: `tc filter show`.
// Generally retunrs nothing if link and parent are not specified.
func FilterList(link Link, parent uint32) ([]Filter, error) {
	req := nl.NewNetlinkRequest(syscall.RTM_GETTFILTER, syscall.NLM_F_DUMP)
	msg := &nl.TcMsg{
		Family: nl.FAMILY_ALL,
		Parent: parent,
	}
	if link != nil {
		base := link.Attrs()
		ensureIndex(base)
		msg.Ifindex = int32(base.Index)
	}
	req.AddData(msg)

	msgs, err := req.Execute(syscall.NETLINK_ROUTE, syscall.RTM_NEWTFILTER)
	if err != nil {
		return nil, err
	}

	var res []Filter
	for _, m := range msgs {
		msg := nl.DeserializeTcMsg(m)

		attrs, err := nl.ParseRouteAttr(m[msg.Len():])
		if err != nil {
			return nil, err
		}

		base := FilterAttrs{
			LinkIndex: int(msg.Ifindex),
			Handle:    msg.Handle,
			Parent:    msg.Parent,
		}
		base.Priority, base.Protocol = MajorMinor(msg.Info)
		base.Protocol = nl.Swap16(base.Protocol)

		var filter Filter
		filterType := ""
		detailed := false
		for _, attr := range attrs {
			switch attr.Attr.Type {
			case nl.TCA_KIND:
				filterType = string(attr.Value[:len(attr.Value)-1])
				switch filterType {
				case "u32":
					filter = &U32{}
				case "fw":
					filter = &Fw{}
				case "bpf":
					filter = &BpfFilter{}
				default:
					filter = &GenericFilter{FilterType: filterType}
				}
			case nl.TCA_OPTIONS:
				data, err := nl.ParseRouteAttr(attr.Value)
				if err != nil {
					return nil, err
				}
				switch filterType {
				case "u32":
					detailed, err = parseU32Data(filter, data)
					if err != nil {
						return nil, err
					}
				case "fw":
					detailed, err = parseFwData(filter, data)
					if err != nil {
						return nil, err
					}
				case "bpf":
					detailed, err = parseBpfData(filter, data)
					if err != nil {
						return nil, err
					}
				}
			}
		}
		// only return the detailed version of the filter
		if detailed {
			*filter.Attrs() = base
			res = append(res, filter)
		}
	}

	return res, nil
}

func encodeActions(attr *nl.RtAttr, actions []Action) error {
	tabIndex := int(nl.TCA_ACT_TAB)

	for _, action := range actions {
		switch action := action.(type) {
		default:
			return fmt.Errorf("unknown action type %s", action.Type())
		case *MirredAction:
			table := nl.NewRtAttrChild(attr, tabIndex, nil)
			tabIndex++
			nl.NewRtAttrChild(table, nl.TCA_ACT_KIND, nl.ZeroTerminated("mirred"))
			aopts := nl.NewRtAttrChild(table, nl.TCA_ACT_OPTIONS, nil)
			nl.NewRtAttrChild(aopts, nl.TCA_MIRRED_PARMS, action.Serialize())
		case *BpfAction:
			table := nl.NewRtAttrChild(attr, tabIndex, nil)
			tabIndex++
			nl.NewRtAttrChild(table, nl.TCA_ACT_KIND, nl.ZeroTerminated("bpf"))
			aopts := nl.NewRtAttrChild(table, nl.TCA_ACT_OPTIONS, nil)
			nl.NewRtAttrChild(aopts, nl.TCA_ACT_BPF_PARMS, action.Serialize())
			nl.NewRtAttrChild(aopts, nl.TCA_ACT_BPF_FD, nl.Uint32Attr(uint32(action.Fd)))
			nl.NewRtAttrChild(aopts, nl.TCA_ACT_BPF_NAME, nl.ZeroTerminated(action.Name))
		}
	}
	return nil
}

func parseActions(tables []syscall.NetlinkRouteAttr) ([]Action, error) {
	var actions []Action
	for _, table := range tables {
		var action Action
		var actionType string
		aattrs, err := nl.ParseRouteAttr(table.Value)
		if err != nil {
			return nil, err
		}
	nextattr:
		for _, aattr := range aattrs {
			switch aattr.Attr.Type {
			case nl.TCA_KIND:
				actionType = string(aattr.Value[:len(aattr.Value)-1])
				// only parse if the action is mirred or bpf
				switch actionType {
				case "mirred":
					action = &MirredAction{}
				case "bpf":
					action = &BpfAction{}
				default:
					break nextattr
				}
			case nl.TCA_OPTIONS:
				adata, err := nl.ParseRouteAttr(aattr.Value)
				if err != nil {
					return nil, err
				}
				for _, adatum := range adata {
					switch actionType {
					case "mirred":
						switch adatum.Attr.Type {
						case nl.TCA_MIRRED_PARMS:
							action.(*MirredAction).TcMirred = *nl.DeserializeTcMirred(adatum.Value)
						}
					case "bpf":
						switch adatum.Attr.Type {
						case nl.TCA_ACT_BPF_PARMS:
							action.(*BpfAction).TcActBpf = *nl.DeserializeTcActBpf(adatum.Value)
						case nl.TCA_ACT_BPF_FD:
							action.(*BpfAction).Fd = int(native.Uint32(adatum.Value[0:4]))
						case nl.TCA_ACT_BPF_NAME:
							action.(*BpfAction).Name = string(adatum.Value[:len(adatum.Value)-1])
						}
					}
				}
			}
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func parseU32Data(filter Filter, data []syscall.NetlinkRouteAttr) (bool, error) {
	native = nl.NativeEndian()
	u32 := filter.(*U32)
	detailed := false
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.TCA_U32_SEL:
			detailed = true
			sel := nl.DeserializeTcU32Sel(datum.Value)
			// only parse if we have a very basic redirect
			if sel.Flags&nl.TC_U32_TERMINAL == 0 || sel.Nkeys != 1 {
				return detailed, nil
			}
		case nl.TCA_U32_ACT:
			tables, err := nl.ParseRouteAttr(datum.Value)
			if err != nil {
				return detailed, err
			}
			u32.Actions, err = parseActions(tables)
			if err != nil {
				return detailed, err
			}
			for _, action := range u32.Actions {
				if action, ok := action.(*MirredAction); ok {
					u32.RedirIndex = int(action.Ifindex)
				}
			}
		}
	}
	return detailed, nil
}

func parseFwData(filter Filter, data []syscall.NetlinkRouteAttr) (bool, error) {
	native = nl.NativeEndian()
	fw := filter.(*Fw)
	detailed := true
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.TCA_FW_MASK:
			fw.Mask = native.Uint32(datum.Value[0:4])
		case nl.TCA_FW_CLASSID:
			fw.ClassId = native.Uint32(datum.Value[0:4])
		case nl.TCA_FW_INDEV:
			fw.InDev = string(datum.Value[:len(datum.Value)-1])
		case nl.TCA_FW_POLICE:
			adata, _ := nl.ParseRouteAttr(datum.Value)
			for _, aattr := range adata {
				switch aattr.Attr.Type {
				case nl.TCA_POLICE_TBF:
					fw.Police = *nl.DeserializeTcPolice(aattr.Value)
				case nl.TCA_POLICE_RATE:
					fw.Rtab = DeserializeRtab(aattr.Value)
				case nl.TCA_POLICE_PEAKRATE:
					fw.Ptab = DeserializeRtab(aattr.Value)
				}
			}
		}
	}
	return detailed, nil
}

func parseBpfData(filter Filter, data []syscall.NetlinkRouteAttr) (bool, error) {
	native = nl.NativeEndian()
	bpf := filter.(*BpfFilter)
	detailed := true
	for _, datum := range data {
		switch datum.Attr.Type {
		case nl.TCA_BPF_FD:
			bpf.Fd = int(native.Uint32(datum.Value[0:4]))
		case nl.TCA_BPF_NAME:
			bpf.Name = string(datum.Value[:len(datum.Value)-1])
		case nl.TCA_BPF_CLASSID:
			bpf.ClassId = native.Uint32(datum.Value[0:4])
		case nl.TCA_BPF_FLAGS:
			flags := native.Uint32(datum.Value[0:4])
			if (flags & nl.TCA_BPF_FLAG_ACT_DIRECT) != 0 {
				bpf.DirectAction = true
			}
		}
	}
	return detailed, nil
}

func AlignToAtm(size uint) uint {
	var linksize, cells int
	cells = int(size / nl.ATM_CELL_PAYLOAD)
	if (size % nl.ATM_CELL_PAYLOAD) > 0 {
		cells++
	}
	linksize = cells * nl.ATM_CELL_SIZE
	return uint(linksize)
}

func AdjustSize(sz uint, mpu uint, linklayer int) uint {
	if sz < mpu {
		sz = mpu
	}
	switch linklayer {
	case nl.LINKLAYER_ATM:
		return AlignToAtm(sz)
	default:
		return sz
	}
}

func CalcRtable(rate *nl.TcRateSpec, rtab [256]uint32, cell_log int, mtu uint32, linklayer int) int {
	bps := rate.Rate
	mpu := rate.Mpu
	var sz uint
	if mtu == 0 {
		mtu = 2047
	}
	if cell_log < 0 {
		cell_log = 0
		for (mtu >> uint(cell_log)) > 255 {
			cell_log++
		}
	}
	for i := 0; i < 256; i++ {
		sz = AdjustSize(uint((i+1)<<uint32(cell_log)), uint(mpu), linklayer)
		rtab[i] = uint32(Xmittime(uint64(bps), uint32(sz)))
	}
	rate.CellAlign = -1
	rate.CellLog = uint8(cell_log)
	rate.Linklayer = uint8(linklayer & nl.TC_LINKLAYER_MASK)
	return cell_log
}

func DeserializeRtab(b []byte) [256]uint32 {
	var rtab [256]uint32
	native := nl.NativeEndian()
	r := bytes.NewReader(b)
	_ = binary.Read(r, native, &rtab)
	return rtab
}

func SerializeRtab(rtab [256]uint32) []byte {
	native := nl.NativeEndian()
	var w bytes.Buffer
	_ = binary.Write(&w, native, rtab)
	return w.Bytes()
}
