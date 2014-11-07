package netlinkAudit

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"
	//"unicode"
	//"strings"
	//"runtime"
)

var ParsedResult AuditStatus
var nextSeqNr uint32
var rulesRetrieved AuditRuleData
var audit_elf = 0

//var MachineStrings []byte
var MachineStrings = []string{"armeb", "armv5tejl", "armv7l", "i386", "i486", "i586", "i686", "ia64", "ppc", "ppc64", "s390", "s390x", "x86_64"}

var MachineS2iS = []int{0, 6, 16, 23, 28, 33, 38, 43, 48, 52, 58, 63, 69}
var MachineS2iI = []int{8, 8, 8, 0, 0, 0, 0, 2, 4, 3, 6, 5, 1}

type AuditStatus struct {
	Mask          uint32 /* Bit mask for valid entries */
	Enabled       uint32 /* 1 = enabled, 0 = disabled */
	Failure       uint32 /* Failure-to-log action */
	Pid           uint32 /* pid of auditd process */
	Rate_limit    uint32 /* messages rate limit (per second) */
	Backlog_limit uint32 /* waiting messages limit */
	Lost          uint32 /* messages lost */
	Backlog       uint32 /* messages waiting in queue */
}

type AuditRuleData struct {
	Flags       uint32 /* AUDIT_PER_{TASK,CALL}, AUDIT_PREPEND */
	Action      uint32 /* AUDIT_NEVER, AUDIT_POSSIBLE, AUDIT_ALWAYS */
	Field_count uint32
	Mask        [AUDIT_BITMASK_SIZE]uint32 /* syscall(s) affected */
	Fields      [AUDIT_MAX_FIELDS]uint32
	Values      [AUDIT_MAX_FIELDS]uint32
	Fieldflags  [AUDIT_MAX_FIELDS]uint32
	Buflen      uint32  /* total length of string fields */
	Buf         [0]byte //[0]string /* string fields buffer */
}

type NetlinkSocket struct {
	fd  int
	lsa syscall.SockaddrNetlink
}

type NetlinkAuditRequest struct {
	Header syscall.NlMsghdr
	Data   []byte
}

// for config
type CMap struct {
	Name string
	Id   int
}

//for fieldtab
type FMap struct {
	Name    string
	Fieldid int
}

// for config
type Config struct {
	Xmap []CMap
}

//for fieldtab
type Field struct {
	Fieldmap []FMap
}

func nativeEndian() binary.ByteOrder {
	var x uint32 = 0x01020304
	if *(*byte)(unsafe.Pointer(&x)) == 0x01 {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

//The recvfrom in go takes only a byte [] to put the data recieved from the kernel that removes the need
//for having a separate audit_reply Struct for recieving data from kernel.
func (rr *NetlinkAuditRequest) ToWireFormat() []byte {
	b := make([]byte, rr.Header.Len)
	*(*uint32)(unsafe.Pointer(&b[0:4][0])) = rr.Header.Len
	*(*uint16)(unsafe.Pointer(&b[4:6][0])) = rr.Header.Type
	*(*uint16)(unsafe.Pointer(&b[6:8][0])) = rr.Header.Flags
	*(*uint32)(unsafe.Pointer(&b[8:12][0])) = rr.Header.Seq
	*(*uint32)(unsafe.Pointer(&b[12:16][0])) = rr.Header.Pid
	b = append(b[:16], rr.Data[:]...) //Important b[:16]
	return b
}

func newNetlinkAuditRequest(proto, family, sizeofData int) *NetlinkAuditRequest {
	rr := &NetlinkAuditRequest{}

	rr.Header.Len = uint32(syscall.NLMSG_HDRLEN + sizeofData)
	rr.Header.Type = uint16(proto)
	rr.Header.Flags = syscall.NLM_F_REQUEST | syscall.NLM_F_ACK
	rr.Header.Seq = atomic.AddUint32(&nextSeqNr, 1) //Autoincrementing Sequence
	return rr
	//	return rr.ToWireFormat()
}

// Round the length of a netlink message up to align it properly.
func nlmAlignOf(msglen int) int {
	return (msglen + syscall.NLMSG_ALIGNTO - 1) & ^(syscall.NLMSG_ALIGNTO - 1)
}

/*
 NLMSG_HDRLEN     ((int) NLMSG_ALIGN(sizeof(struct nlmsghdr)))
 NLMSG_LENGTH(len) ((len) + NLMSG_HDRLEN)
 NLMSG_SPACE(len) NLMSG_ALIGN(NLMSG_LENGTH(len))
 NLMSG_DATA(nlh)  ((void*)(((char*)nlh) + NLMSG_LENGTH(0)))
 NLMSG_NEXT(nlh,len)      ((len) -= NLMSG_ALIGN((nlh)->nlmsg_len), \
                                    (struct nlmsghdr*)(((char*)(nlh)) + NLMSG_ALIGN((nlh)->nlmsg_len)))
 NLMSG_OK(nlh,len) ((len) >= (int)sizeof(struct nlmsghdr) && \
                             (nlh)->nlmsg_len >= sizeof(struct nlmsghdr) && \
                             (nlh)->nlmsg_len <= (len))
*/

func ParseAuditNetlinkMessage(b []byte) ([]syscall.NetlinkMessage, error) {

	var msgs []syscall.NetlinkMessage
	h, dbuf, dlen, err := netlinkMessageHeaderAndData(b)
	if err != nil {
		log.Println("Error in parsing")
		return nil, err
	}

	m := syscall.NetlinkMessage{Header: *h, Data: dbuf[:int(h.Len) /* -syscall.NLMSG_HDRLEN*/]}
	msgs = append(msgs, m)
	b = b[dlen:]

	return msgs, nil
}

func netlinkMessageHeaderAndData(b []byte) (*syscall.NlMsghdr, []byte, int, error) {

	h := (*syscall.NlMsghdr)(unsafe.Pointer(&b[0]))
	if int(h.Len) < syscall.NLMSG_HDRLEN || int(h.Len) > len(b) {
		foo := int32(nativeEndian().Uint32(b[0:4]))
		log.Println("Headerlength with ", foo, b[0]) //!bug ! FIX THIS
		log.Println("Error due to....HDRLEN:", syscall.NLMSG_HDRLEN, " Header Length:", h.Len, " Length of BYTE Array:", len(b))
		return nil, nil, 0, syscall.EINVAL
	}
	return h, b[syscall.NLMSG_HDRLEN:], nlmAlignOf(int(h.Len)), nil
}

// This function makes a conncetion with kernel space and is to be used for all further socket communication
func GetNetlinkSocket() (*NetlinkSocket, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_AUDIT)
	if err != nil {
		return nil, err
	}
	s := &NetlinkSocket{
		fd: fd,
	}
	s.lsa.Family = syscall.AF_NETLINK
	s.lsa.Groups = 0
	s.lsa.Pid = 0 //Kernel space pid is always set to be 0

	if err := syscall.Bind(fd, &s.lsa); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return s, nil
}

//To end the socket conncetion
func (s *NetlinkSocket) Close() {
	syscall.Close(s.fd)
}

func (s *NetlinkSocket) Send(request *NetlinkAuditRequest) error {
	if err := syscall.Sendto(s.fd, request.ToWireFormat(), 0, &s.lsa); err != nil {
		return err
	}
	return nil
}

func (s *NetlinkSocket) Receive(bytesize int, block int) ([]syscall.NetlinkMessage, error) {
	rb := make([]byte, bytesize)
	nr, _, err := syscall.Recvfrom(s.fd, rb, 0|block)
	//nr, _, err := syscall.Recvfrom(s, rb, syscall.MSG_PEEK|syscall.MSG_DONTWAIT)

	if err != nil {
		return nil, err
	}
	if nr < syscall.NLMSG_HDRLEN {
		return nil, syscall.EINVAL
	}
	rb = rb[:nr]
	return ParseAuditNetlinkMessage(rb)
}

//should it be changed to HandleAck ?
func AuditGetReply(s *NetlinkSocket, bytesize, block int, seq uint32) error {
done:
	for {
		msgs, err := s.Receive(bytesize, block) //ParseAuditNetlinkMessage(rb)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			lsa, err := syscall.Getsockname(s.fd)
			if err != nil {
				return err
			}
			switch v := lsa.(type) {
			case *syscall.SockaddrNetlink:

				if m.Header.Seq != seq {
					return fmt.Errorf("Wrong Seq nr %d, expected %d", m.Header.Seq, seq)
				}
				if m.Header.Pid != v.Pid {
					return fmt.Errorf("Wrong pid %d, expected %d", m.Header.Pid, v.Pid)
				}
			default:
				return syscall.EINVAL
			}

			if m.Header.Type == syscall.NLMSG_DONE {
				break done
			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				error := int32(nativeEndian().Uint32(m.Data[0:4]))
				if error == 0 {
					log.Println("Acknowledged!!")
					break done
				} else {
					log.Println("NLMSG_ERROR Received..")
				}
				break done
			}
			if m.Header.Type == AUDIT_GET {
				log.Println("AUDIT_GET")
				break done
			}
		}
	}
	return nil
}

func AuditSetEnabled(s *NetlinkSocket) error {
	var status AuditStatus
	status.Enabled = 1
	status.Mask = AUDIT_STATUS_ENABLED
	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), status)
	if err != nil {
		log.Println("binary.Write failed:", err)
		return err
	}

	wb := newNetlinkAuditRequest(AUDIT_SET, syscall.AF_NETLINK, int(unsafe.Sizeof(status)))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}

	// Receiving IN JUST ONE TRY
	err = AuditGetReply(s, syscall.Getpagesize(), 0, wb.Header.Seq)
	if err != nil {
		return err
	}
	return nil
}

func AuditIsEnabled(s *NetlinkSocket) error {
	wb := newNetlinkAuditRequest(AUDIT_GET, syscall.AF_NETLINK, 0)

	if err := s.Send(wb); err != nil {
		return err
	}

done:
	for {
		//Make the rb byte bigger because of large messages from Kernel doesn't fit in 4096
		msgs, err := s.Receive(MAX_AUDIT_MESSAGE_LENGTH, syscall.MSG_DONTWAIT)
		if err != nil {
			return err
		}

		for _, m := range msgs {
			lsa, er := syscall.Getsockname(s.fd)
			if er != nil {
				return nil
			}
			switch v := lsa.(type) {
			case *syscall.SockaddrNetlink:

				if m.Header.Seq != uint32(wb.Header.Seq) || m.Header.Pid != v.Pid {
					return syscall.EINVAL
				}
			default:
				return syscall.EINVAL
			}
			if m.Header.Type == syscall.NLMSG_DONE {
				log.Println("Done")
				break done

			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				log.Println("NLMSG_ERROR Received..")
			}
			if m.Header.Type == AUDIT_GET {
				//Convert the data part written to AuditStatus struct
				b := m.Data[:]
				buf := bytes.NewBuffer(b)
				var dumm AuditStatus
				err = binary.Read(buf, nativeEndian(), &dumm)
				ParsedResult = dumm
				break done
			}
		}
	}
	return nil
}

func AuditSetPid(s *NetlinkSocket, pid uint32 /*,Wait mode WAIT_YES | WAIT_NO */) error {
	var status AuditStatus
	status.Mask = AUDIT_STATUS_PID
	status.Pid = pid
	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), status)
	if err != nil {
		log.Println("binary.Write failed:", err)
		return err
	}

	wb := newNetlinkAuditRequest(AUDIT_SET, syscall.AF_NETLINK, int(unsafe.Sizeof(status)))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}

	err = AuditGetReply(s, syscall.Getpagesize(), 0, wb.Header.Seq)
	if err != nil {
		return err
	}
	//Polling in GO Is it needed ?
	return nil
}

func auditWord(nr int) uint32 {
	audit_word := (uint32)((nr) / 32)
	return (uint32)(audit_word)
}

func auditBit(nr int) uint32 {
	audit_bit := 1 << ((uint32)(nr) - auditWord(nr)*32)
	return (uint32)(audit_bit)
}

func AuditRuleSyscallData(rule *AuditRuleData, scall int) error {
	word := auditWord(scall)
	bit := auditBit(scall)

	if word >= AUDIT_BITMASK_SIZE-1 {
		log.Println("Word Size greater than AUDIT_BITMASK_SIZE")
		return syscall.EINVAL
	}
	rule.Mask[word] |= bit
	return nil
}

/*
func AuditWatchRuleData(s *NetlinkSocket, rule *AuditRuleData, path []byte) error {
	rule.Flags = uint32(AUDIT_FILTER_EXIT)
	rule.Action = uint32(AUDIT_ALWAYS)
	// set mask
	rule.Field_count = uint32(2)
	rule.Fields[0] = uint32(105)
	rule.Values[0] = uint32(len(path))
	rule.Fieldflags[0] = uint32(AUDIT_EQUAL)
	rule.Buflen = uint32(len(path))
	rule.Buf = append(rule.Buf[:], path[:]...)

	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), *rule)
	if err != nil {
		log.Println("binary.Write failed:", err)
		return err
	}

	wb := newNetlinkAuditRequest(AUDIT_ADD_RULE, syscall.AF_NETLINK, int(buff.Len())+int(rule.Buflen))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}

	return nil
}
*/

func AuditAddRuleData(s *NetlinkSocket, rule *AuditRuleData, flags int, action int) error {

	if flags == AUDIT_FILTER_ENTRY {
		log.Println("Use of entry filter is deprecated")
		return nil
	}

	rule.Flags = uint32(flags)
	rule.Action = uint32(action)

	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), *rule)
	if err != nil {
		log.Println("binary.Write failed:", err)
		return err
	}
	wb := newNetlinkAuditRequest(AUDIT_ADD_RULE, syscall.AF_NETLINK, int(buff.Len())+int(rule.Buflen))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}

	if err != nil {
		log.Println("Error sending add rule data request ()")
		return err
	}
	return nil
}
func isDone(msgchan chan string, errchan chan error, done <-chan bool) bool {
	var d bool
	select {
	case d = <-done:
		close(msgchan)
		close(errchan)
	default:
	}
	return d
}

func GetreplyWithoutSync(s *NetlinkSocket) {
	f, err := os.OpenFile("log", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0660)
	if err != nil {
		log.Println("Error Creating File!!")
		return
	}
	defer f.Close()
	for {
		rb := make([]byte, MAX_AUDIT_MESSAGE_LENGTH)
		nr, _, err := syscall.Recvfrom(s.fd, rb, 0)
		if err != nil {
			log.Println("Error While Recieving !!")
			continue
		}
		if nr < syscall.NLMSG_HDRLEN {
			log.Println("Message Too Short!!")
			continue
		}

		rb = rb[:nr]
		msgs, err := ParseAuditNetlinkMessage(rb)

		if err != nil {
			log.Println("Not Parsed Successfuly !!")
			continue
		}
		for _, m := range msgs {
			//Decide on various message Types
			if m.Header.Type == syscall.NLMSG_DONE {
				log.Println("Done")
			} else if m.Header.Type == syscall.NLMSG_ERROR {
				err := int32(nativeEndian().Uint32(m.Data[0:4]))
				if err == 0 {
					//Acknowledgement from kernel
					log.Println("Ack")
				} else {
					log.Println("NLMSG_ERROR...")
				}
			} else if m.Header.Type == AUDIT_GET {
				log.Println("AUDIT_GET")
			} else if m.Header.Type == AUDIT_FIRST_USER_MSG {
				log.Println("AUDIT_FIRST_USER_MSG")
			} else if m.Header.Type == AUDIT_SYSCALL {
				log.Println("Syscall Event")
				log.Println(string(m.Data[:]))
				_, err := f.WriteString(string(m.Data[:]) + "\n")
				if err != nil {
					log.Println("Writing Error!!")
				}
			} else if m.Header.Type == AUDIT_CWD {
				log.Println("CWD Event")
				log.Println(string(m.Data[:]))
				_, err := f.WriteString(string(m.Data[:]) + "\n")
				if err != nil {
					log.Println("Writing Error!!")
				}

			} else if m.Header.Type == AUDIT_PATH {
				log.Println("Path Event")
				log.Println(string(m.Data[:]))
				_, err := f.WriteString(string(m.Data[:]) + "\n")
				if err != nil {
					log.Println("Writing Error!!")
				}

			} else if m.Header.Type == AUDIT_EOE {
				log.Println("Event Ends ", string(m.Data[:]))
			} else if m.Header.Type == AUDIT_CONFIG_CHANGE {
				log.Println("Config Change ", string(m.Data[:]))
				_, err := f.WriteString(string(m.Data[:]) + "\n")
				if err != nil {
					log.Println("Writing Error!!")
				}
			} else {
				log.Println("Unknown: ", m.Header.Type)
			}
		}
	}
}

func Getreply(s *NetlinkSocket, done <-chan bool, msgchan chan string, errchan chan error) {
	for {
		rb := make([]byte, MAX_AUDIT_MESSAGE_LENGTH)
		nr, _, err := syscall.Recvfrom(s.fd, rb, 0)
		if isDone(msgchan, errchan, done) {
			return
		}
		if err != nil {
			log.Println("Error While Recieving !!")
			errchan <- err
			continue
		}
		if nr < syscall.NLMSG_HDRLEN {
			log.Println("Message Too Short!!")
			errchan <- syscall.EINVAL
			continue
		}

		rb = rb[:nr]
		msgs, err := ParseAuditNetlinkMessage(rb)

		if err != nil {
			log.Println("Not Parsed Successfuly !!")
			errchan <- err
			continue
		}
		for _, m := range msgs {
			//Decide on various message Types
			if m.Header.Type == syscall.NLMSG_DONE {
				log.Println("Done")
			} else if m.Header.Type == syscall.NLMSG_ERROR {
				err := int32(nativeEndian().Uint32(m.Data[0:4]))
				if err == 0 {
					//Acknowledgement from kernel
					log.Println("Ack")
				} else {
					log.Println("NLMSG_ERROR")
				}
			} else if m.Header.Type == AUDIT_GET {
				log.Println("AUDIT_GET")
			} else if m.Header.Type == AUDIT_FIRST_USER_MSG {
				log.Println("AUDIT_FIRST_USER_MSG")
			} else if m.Header.Type == AUDIT_SYSCALL {
				msgchan <- string(m.Data[:])
			} else if m.Header.Type == AUDIT_CWD {
				msgchan <- string(m.Data[:])
			} else if m.Header.Type == AUDIT_PATH {
				msgchan <- string(m.Data[:])
			} else if m.Header.Type == AUDIT_EOE {
				//			msgchan <- string(m.Data[:])
				log.Println("Event Ends ", string(m.Data[:]))
			} else if m.Header.Type == AUDIT_CONFIG_CHANGE {
				msgchan <- string(m.Data[:])
			} else {
				log.Println("Unknown: ", m.Header.Type)
			}
		}
	}

}

// List all rules
// TODO: this funcion needs a lot of work to print actual rules
func ListAllRules(s *NetlinkSocket) {
	wb := newNetlinkAuditRequest(AUDIT_LIST_RULES, syscall.AF_NETLINK, 0)
	if err := s.Send(wb); err != nil {
		log.Print("Error:", err)
	}

done:
	for {
		msgs, err := s.Receive(MAX_AUDIT_MESSAGE_LENGTH, syscall.MSG_DONTWAIT)
		if err != nil {
			log.Println("ERROR while receiving rules:", err)
		}

		for _, m := range msgs {
			lsa, er := syscall.Getsockname(s.fd)
			if er != nil {
				log.Println("ERROR:", er)
			}
			switch v := lsa.(type) {
			case *syscall.SockaddrNetlink:
				if m.Header.Seq != uint32(wb.Header.Seq) || m.Header.Pid != v.Pid {
					log.Println("ERROR:", syscall.EINVAL)
				}
			default:
				log.Println("ERROR:", syscall.EINVAL)
			}

			if m.Header.Type == syscall.NLMSG_DONE {
				log.Println("All rules deleted")
				break done
			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				log.Println("NLMSG_ERROR")
			}
			if m.Header.Type == AUDIT_LIST_RULES {
				b := m.Data[:]
				buf := bytes.NewBuffer(b)
				var rules AuditRuleData
				err = binary.Read(buf, nativeEndian(), &rules)
				// TODO : save all rules to an array so delete all rules function can use this
				rulesRetrieved = rules
			}
		}
	}
}

//Delete Rule Data Function
func AuditDeleteRuleData(s *NetlinkSocket, rule *AuditRuleData, flags uint32, action uint32) error {
	var sizePurpose AuditRuleData
	if flags == AUDIT_FILTER_ENTRY {
		log.Println("Error in delete")
		return nil
	}
	rule.Flags = flags
	rule.Action = action

	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), *rule)
	if err != nil {
		log.Println("binary.Write failed:", err)
		return err
	}
	wb := newNetlinkAuditRequest(AUDIT_DEL_RULE, syscall.AF_NETLINK, int(unsafe.Sizeof(sizePurpose))+int(rule.Buflen))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}
	return nil
}

// This function Deletes all rules
func DeleteAllRules(s *NetlinkSocket) {
	wb := newNetlinkAuditRequest(AUDIT_LIST_RULES, syscall.AF_NETLINK, 0)
	if err := s.Send(wb); err != nil {
		log.Print("Error:", err)
	}

done:
	for {
		//Make the rb byte bigger because of large messages from Kernel doesn't fit in 4096
		msgs, err := s.Receive(MAX_AUDIT_MESSAGE_LENGTH, syscall.MSG_DONTWAIT)
		if err != nil {
			log.Println("ERROR while receiving rules:", err)
		}

		for _, m := range msgs {
			lsa, er := syscall.Getsockname(s.fd)
			if er != nil {
				log.Println("ERROR:", er)
			}
			switch v := lsa.(type) {
			case *syscall.SockaddrNetlink:
				if m.Header.Seq != uint32(wb.Header.Seq) || m.Header.Pid != v.Pid {
					log.Println("ERROR:", syscall.EINVAL)
				}
			}

			if m.Header.Type == syscall.NLMSG_DONE {
				log.Println("Deleting Done!")
				break done

			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				log.Println("NLMSG_ERROR\n")
			}
			if m.Header.Type == AUDIT_LIST_RULES {
				b := m.Data[:]
				buf := bytes.NewBuffer(b)
				var rules AuditRuleData
				err = binary.Read(buf, nativeEndian(), &rules)
				AuditDeleteRuleData(s, &rules, rules.Flags, rules.Action)
			}
		}
	}
}

// function that sets each rule after reading configuration file
func SetRules(s *NetlinkSocket) {

	//var rule AuditRuleData
	//AuditWatchRuleData(s, &rule, []byte("/etc/passwd"))

	// Load all rules
	content, err := ioutil.ReadFile("netlinkAudit/audit.rules.json")
	if err != nil {
		log.Print("Error:", err)
	}

	var rules interface{}
	err = json.Unmarshal(content, &rules)

	m := rules.(map[string]interface{})

	if _, ok := m["delete"]; ok {
		//First Delete All rules and then add rules
		log.Println("Deleting all rules")
		DeleteAllRules(s)
	}

	for k, v := range m {
		switch k {
		case "custom_rule":
			vi := v.([]interface{})
			for ruleNo := range vi {
				rule := vi[ruleNo].(map[string]interface{})
				for l, m := range rule {
					switch l {
					case "action":
						//TODO: handle actions case here
						action := m.([]interface{})
						log.Println("actions are : ", action[0])
					case "fields":
						//TODO: handle fields case here
						fields := m.([]interface{})
						for _, q := range fields {
							log.Println("fields are", q)
						}
					}
				}
			}
		case "syscall_rules":
			vi := v.([]interface{})
			for sruleNo := range vi {
				srule := vi[sruleNo].(map[string]interface{})

				// Load x86 map and fieldtab.json
				content2, err := ioutil.ReadFile("netlinkAudit/audit_x86.json")
				if err != nil {
					log.Print("Error:", err)
				}
				content3, err := ioutil.ReadFile("netlinkAudit/fieldtab.json")
				if err != nil {
					log.Print("Error:", err)
				}

				var conf Config
				var fieldmap Field
				err = json.Unmarshal([]byte(content2), &conf)
				if err != nil {
					log.Print("Error:", err)
				}
				err = json.Unmarshal([]byte(content3), &fieldmap)
				if err != nil {
					log.Print("Error:", err)
				}

				for l := range conf.Xmap {
					if conf.Xmap[l].Name == srule["name"] {
						// set rules
						log.Println("setting syscall rule", conf.Xmap[l].Name)
						var foo AuditRuleData
						AuditRuleSyscallData(&foo, conf.Xmap[l].Id)
						actions := srule["action"].([]interface{})
						log.Println(actions)
						//NOW APPLY ACTIONS ON SYSCALLS by separating the filters i.e exit from action i.e. always

						for _, field := range srule["fields"].([]interface{}) {
							fieldval := field.(map[string]interface{})["value"]
							op := field.(map[string]interface{})["op"]
							fieldname := field.(map[string]interface{})["name"]
							log.Println(fieldval, op, fieldname)
							// AuditRuleFieldPairData(&foo,fieldval,op,fieldname,fieldmap)
							//SEND flags in above function as " filter & AUDIT_BIT_MASK
						}
						foo.Fields[foo.Field_count] = AUDIT_ARCH
						foo.Fieldflags[foo.Field_count] = AUDIT_EQUAL
						foo.Values[foo.Field_count] = AUDIT_ARCH_X86_64
						foo.Field_count++

						AuditAddRuleData(s, &foo, AUDIT_FILTER_EXIT, AUDIT_ALWAYS)
					}
				}
			}
		}
	}
}

/*
func  AuditRuleFieldPairData(rule AuditRuleData,fieldval int, op string, fieldname string ,fieldmap Field , flags int) error {

	if rule.Field_count >= (AUDIT_MAX_FIELDS - 1) {
		err :=
		return err
	}
	var _audit_syscalladded, _audit_permadded int
	_audit_syscalladded = 0
	_audit_permadded = 0
	var _audit_permadded =1 int

	if v == "nt_eq" {
		op = AUDIT_NOT_EQUAL
	} else if v == "gt_or_eq" {
		op = AUDIT_GREATER_THAN_OR_EQUAL
	} else if v == "lt_or_eq" {
		op = AUDIT_LESS_THAN_OR_EQUAL
	} else if v == "and_eq" {
		op = AUDIT_BIT_TEST
	} else if v == "eq" {
		op = AUDIT_EQUAL
	} else if v == "gt" {
		op = AUDIT_GREATER_THAN
	} else if v == "lt" {
		op = AUDIT_LESS_THAN
	} else if ( (v == "and" {
		op = AUDIT_BIT_MASK
	}

	// check against  the field["actions"] here and set fieldid
	fieldid := 0
	for f := range fieldmap.Fieldmap {
		if fieldmap.Fieldmap[f].Name == fieldname {
			fieldid = (uint32)(fieldmap.Fieldmap[f].Fieldid)
		}
	}

	// check against the field["actions"] here and set fieldid
	opval := 0
	for f := range fieldmap.Fieldmap {
		if fieldmap.Fieldmap[f].Name == op {
			opval = //DO SMTH ELSE HERE
		}
	}

	//set field and op
	rule.Fields[foo.Field_count] = fieldid;
	rule.Fieldflags[foo.Field_count] = opval;

	if t == "task"{
		para_one = AUDIT_FILTER_TASK
	}
	else if t == "entry" {
		para_one = AUDIT_FILTER_ENTRY
	}
	else if t == "exit"{
		para_one = AUDIT_FILTER_EXIT
	}
	else if t == "user"{
		para_one = AUDIT_FILTER_USER
-	}
	else if t == "exclude" {
		para_one = AUDIT_FILTER_EXCLUDE
		//exclude = 1;
	}

	if m == "never"{
		para_two = AUDIT_NEVER
	}
	else if m == "possible" {
		para_two = AUDIT_POSSIBLE
	}
	else if m == "always"{
		para_two = AUDIT_ALWAYS
	}

	//TODO :Now loop over the field value and set foo.Values[foo.Field_count] accordingly
	//ALSO : Save flags in a variable i.e always,exit SEE static int lookup_filter(const char *str, int *filter)
	//and static int lookup_action(const char *str, int *act) in auditctl.c
	//filters are like entry,exit,task, and action in always or never
	switch fieldid {
    	case AUDIT_UID:
		case AUDIT_UID:
		case AUDIT_EUID:
		case AUDIT_SUID:
		case AUDIT_FSUID:
		case AUDIT_LOGINUID:
		case AUDIT_OBJ_UID:
		case AUDIT_OBJ_GID:
				vlen = len(v)
				if unicode.IsDigit(v){

					rule.Values[rule.Field_count] = v;
				}
				else {
					if vlen >= 2 and strings.Contains(v, "-")//look at it
						rule.Values[rule.Field_count] = v;
					else if runtime·strcmp(v, "unset") == 0 {
						rule->values[rule->field_count] = 4294967295;
						else{
							log.Println("error",v);
						}
					}
				}
			//
			//IF NOT DIGITS THEN DO WE NEED audit_name_to_uid for audit-go ?
		case AUDIT_GID:
		case AUDIT_EGID:
		case AUDIT_SGID:
		case AUDIT_FSGID:

			//rule.Values[rule.Field_count] = uint32(fieldval)

			//IF DIGITS THEN
			if unicode.IsDigit(v){

					rule.Values[rule.Field_count] = v;
			}
			else {
					fmt.Println("error", v);
					//return -2;
				}
			}

		case AUDIT_EXIT:

			if Flags != AUDIT_FILTER_EXIT{
				fmt.Println("Something Went Wrong")
			}
			//rule.Values[rule.Field_count] = fieldval


			vlen = len(v)
			if unicode.IsDigit(v){

					rule.Values[rule.Field_count] = v;
				}
				else {
					if vlen >= 2 and strings.Contains(v, "-")//look at it
						rule.Values[rule.Field_count] = v;
					else {
							fmt.Println("error",v);
						}
					}

			//error handling part need to be done
			//else {
			//	rule->values[rule->field_count] = //SEE HERE
			//			audit_name_to_errno(v);
			//	if (rule->values[rule->field_count] == 0)
			//		return -15;
			//}
			//break;

		case AUDIT_MSGTYPE:
			//DO we need this type ?

			if (Flags != AUDIT_FILTER_EXCLUDE && Flags != AUDIT_FILTER_USER){
				fmt.Println("SOmething went wrong")
			}

			vlen = len(v)
			if unicode.IsDigit(v){
					rule.Values[rule.Field_count] = v;
			}

			/*
			if unicode.IsDigit(v){
					rule.Values[rule.Field_count] = v;
			}
			else if (vlen >= 2 && *(v)=='-' &&
						(isdigit((char)*(v+1))))
				rule->values[rule->field_count] =
					strtol(v, NULL, 0);
			else {
				rule->values[rule->field_count] =
						audit_name_to_errno(v);
				if (rule->values[rule->field_count] == 0)
					return -15;
			}

			//Error handling part after initial work is done
			//else
			//	if (audit_name_to_msg_type(v) > 0)
			//		rule->values[rule->field_count] = //SEE ERROR handling HERE
			//			audit_name_to_msg_type(v);
			//	else
			//		return -8;
			//break;

		case AUDIT_OBJ_USER:
		case AUDIT_OBJ_ROLE:
		case AUDIT_OBJ_TYPE:
		case AUDIT_OBJ_LEV_LOW:
		case AUDIT_OBJ_LEV_HIGH:
		case AUDIT_WATCH:
		case AUDIT_DIR: //DETERMINE WHY they are doing this
			if Flags != AUDIT_FILTER_EXIT{
				fmt.Println("error")
			}
			if Fields == AUDIT_WATCH || Fields == AUDIT_DIR
				_audit_permadded = 1;

		case AUDIT_SUBJ_USER:
		case AUDIT_SUBJ_ROLE:
		case AUDIT_SUBJ_TYPE:
		case AUDIT_SUBJ_SEN:
		case AUDIT_SUBJ_CLR:
		case AUDIT_FILTERKEY:
		//IF And only if a syscall is added or a permisission is added then this field should be set
		//TODO : Get our own to determine the above conditions

			if Field == AUDIT_FILTERKEY and !(_audit_syscalladded or _audit_permadded){
                  	fmt.Println("error in filetr");
            }
			vlen = len(v);
			if Field == AUDIT_FILTERKEY and vlen > AUDIT_MAX_KEY_LEN {
				fmt.Println("Error here")
			}
			else if vlen > PATH_MAX{
				fmt.Println("Error 11")
			}
			rule.Values[rule.Field_count] = vlen
			offset = rule.Buflen
			rule.buflen += vlen
			//*RULEP IS THE RULEDATA STRUCT POINTER
			*rulep = realloc(rule, unsafe.SizeOf(*rule) + rule.buflen);
			if (*rulep == NULL) {
				fmt.Println("error rulep")
			} else {
				rule = *rulep
			}
			v := &rule.buf[offset]
			//strncpy(&rule.buf[offset], v, vlen);

			break
		case AUDIT_ARCH:
			//A syscall should not be added before doing this if this field is applied
			//unsigned int _audit_elf = 0U;
			if (_audit_syscalladded)
				fmt.Println("some error arch")
			if (!(op == AUDIT_NOT_EQUAL || op == AUDIT_EQUAL))
				fmt.Println("Some error occured")

			if unicode.IsDigit(v) {
				var machine int

				errno := 0
				_audit_elf = v
				if errno{
					fmt.Println(errno)
				}

				machine = audit_elf_to_machine(_audit_elf)
				if (machine < 0)
					fmt.Println("Zero machine")
			}
			else {
				const char *arch=v;
				unsigned int machine, elf;
				machine = audit_determine_machine(arch);

				elf = audit_machine_to_elf(machine);
				if (elf == 0)
					return -5;

				_audit_elf = elf;
			}
			rule->values[rule->field_count] = _audit_elf;
			_audit_archadded = 1;
			break;

		case AUDIT_PERM:
			//DECIDE ON VARIOUS ERROR TYPES
			if (flags != AUDIT_FILTER_EXIT)
				return -7;
			else if (op != AUDIT_EQUAL)
				return -13;
			else {
				unsigned int i, len, val = 0;

				len = strlen(v);
				if (len > 4)
					return -11;

				for (i = 0; i < len; i++) {
					switch (tolower(v[i])) {
						case 'r':
							val |= AUDIT_PERM_READ;
							break;
						case 'w':
							val |= AUDIT_PERM_WRITE;
							break;
						case 'x':
							val |= AUDIT_PERM_EXEC;
							break;
						case 'a':
							val |= AUDIT_PERM_ATTR;
							break;
						default:
							return -14;
					}
				}
				rule->values[rule->field_count] = val;
			}
			break;
		case AUDIT_FILETYPE:

			if (!(flags == AUDIT_FILTER_EXIT || flags == AUDIT_FILTER_ENTRY))
				return -17;
			rule->values[rule->field_count] =
				audit_name_to_ftype(v);
			if ((int)rule->values[rule->field_count] < 0) {
				return -16;
			}
			break;

		case AUDIT_ARG0...AUDIT_ARG3: //ARGUMENTS GIVEN
			vlen = strlen(v);
			if (isdigit((char)*(v)))
				rule->values[rule->field_count] =
					strtoul(v, NULL, 0);
			else if (vlen >= 2 && *(v)=='-' &&
						(isdigit((char)*(v+1))))
				rule->values[rule->field_count] =
					strtol(v, NULL, 0);
			else
				return -21;
			break;
		case AUDIT_DEVMAJOR...AUDIT_INODE:
		case AUDIT_SUCCESS:
			if (flags != AUDIT_FILTER_EXIT)
				return -7;

		default:
			if (field == AUDIT_INODE) {
				if (!(op == AUDIT_NOT_EQUAL ||
							op == AUDIT_EQUAL))
					return -13;
			}

			if (field == AUDIT_PPID && !(flags == AUDIT_FILTER_EXIT
				|| flags == AUDIT_FILTER_ENTRY))
				return -17;

			if (!isdigit((char)*(v)))
				return -21;

			if (field == AUDIT_INODE)
				rule->values[rule->field_count] =
					strtoul(v, NULL, 0);
			else
				rule->values[rule->field_count] =
					strtol(v, NULL, 0);
			break;
    }
}
*/
