package stratum

import (
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"git.dero.io/Nelbert442/dero-golang-pool/util"
)

const (
	paramAddr = iota
	paramWID  = iota
	paramPID  = iota
	paramDiff = iota
)

var noncePattern *regexp.Regexp

func init() {
	noncePattern, _ = regexp.Compile("^[0-9a-f]{8}$")
}

func (s *StratumServer) handleLoginRPC(cs *Session, params *LoginParams) (*JobReply, *ErrorReply) {

	var id string
	// Login validation / splitting optimized by Peppinux (https://github.com/peppinux)
	address, workID, paymentid, fixDiff := s.splitLoginString(params.Login)

	// PaymentID Length Validation
	if paymentid != "" {
		if len(paymentid) == 16 || len(paymentid) == 64 {
			_, err := hex.DecodeString(paymentid)

			if err != nil {
				log.Printf("Invalid paymentID %s used for login by %s - %s", paymentid, cs.ip, params.Login)
				return nil, &ErrorReply{Code: -1, Message: "Invalid paymentID used for login"}
			}
		} else {
			log.Printf("Invalid paymentID %s used for login by %s - %s", paymentid, cs.ip, params.Login)
			return nil, &ErrorReply{Code: -1, Message: "Invalid paymentID used for login"}
		}

		// Adding paymentid onto the worker id because later when payments are processed, it's easily identifiable what is the paymentid to supply for creating tx etc.
		id = address + "+" + paymentid
	}

	// If workID is used, then append with ~, this will be easily deciphered later for payments. In future, could store id and values separately so that address payout is clearer
	if workID != address && workID != "" {
		if id != "" {
			// If id is not "" (default value upon var), then it must have a paymentid and have been set. So append ~workID to it
			id = id + s.config.Stratum.WorkerID.AddressSeparator + workID
		} else {
			// If id is "" (default value upon var), then it does not have paymentid and append ~workID to address normally
			id = address + s.config.Stratum.WorkerID.AddressSeparator + workID
		}
	} else {
		if id == "" {
			// If id is "" (default value upon var), then it does not have a paymentid and in this else doesn't have workid, so set id to address for default. Otherwise, id has already been set
			id = address
		}
	}

	if !util.ValidateAddress(address, s.config.Address) {
		log.Printf("Invalid address %s used for login by %s", address, cs.ip)
		return nil, &ErrorReply{Code: -1, Message: "Invalid address used for login"}
	}

	t := s.currentBlockTemplate()
	if t == nil {
		return nil, &ErrorReply{Code: -1, Message: "Job not ready"}
	}

	miner, ok := s.miners.Get(id)
	if !ok {
		log.Printf("Registering new miner: %s@%s, Address: %s, PaymentID: %s, fixedDiff: %v", id, cs.ip, address, paymentid, fixDiff)
		miner = NewMiner(id, address, paymentid, fixDiff, cs.ip)
		s.registerMiner(miner)
	}

	log.Printf("Miner connected %s@%s, Address: %s, PaymentID: %s, fixedDiff: %v", id, cs.ip, address, paymentid, fixDiff)

	s.registerSession(cs)
	miner.heartbeat()

	// Initially set cs.difficulty. If there's no fixDiff defined, inside of cs.getJob the diff target will be set to cs.endpoint.difficulty, otherwise will be set to fixDiff (as long as it's above min diff in config)
	cs.difficulty = int64(fixDiff)

	log.Printf("[handleGetJobRPC] getJob: %v", cs.getJob(t))
	return &JobReply{Id: id, Job: cs.getJob(t), Status: "OK"}, nil
}

func (s *StratumServer) handleGetJobRPC(cs *Session, params *GetJobParams) (*JobReplyData, *ErrorReply) {
	miner, ok := s.miners.Get(params.Id)
	if !ok {
		return nil, &ErrorReply{Code: -1, Message: "Unauthenticated"}
	}
	t := s.currentBlockTemplate()
	if t == nil || s.isSick() {
		return nil, &ErrorReply{Code: -1, Message: "Job not ready"}
	}
	miner.heartbeat()
	log.Printf("[handleGetJobRPC] getJob: %v", cs.getJob(t))
	return cs.getJob(t), nil
}

func (s *StratumServer) handleSubmitRPC(cs *Session, params *SubmitParams) (*StatusReply, *ErrorReply) {
	miner, ok := s.miners.Get(params.Id)
	if !ok {
		return nil, &ErrorReply{Code: -1, Message: "Unauthenticated"}
	}
	miner.heartbeat()

	job := cs.findJob(params.JobId)
	if job == nil {
		return nil, &ErrorReply{Code: -1, Message: "Invalid job id"}
	}

	if !noncePattern.MatchString(params.Nonce) {
		return nil, &ErrorReply{Code: -1, Message: "Malformed nonce"}
	}
	nonce := strings.ToLower(params.Nonce)
	exist := job.submit(nonce)
	if exist {
		atomic.AddInt64(&miner.invalidShares, 1)
		return nil, &ErrorReply{Code: -1, Message: "Duplicate share"}
	}

	t := s.currentBlockTemplate()
	if job.height != t.Height {
		log.Printf("Stale share for height %d from %s@%s", job.height, miner.id, cs.ip)
		atomic.AddInt64(&miner.staleShares, 1)
		return nil, &ErrorReply{Code: -1, Message: "Block expired"}
	}

	validShare, minerOutput := miner.processShare(s, cs, job, t, nonce, params)
	if !validShare {
		return nil, &ErrorReply{Code: -1, Message: minerOutput}
	}
	return &StatusReply{Status: "OK"}, nil
}

func (s *StratumServer) handleUnknownRPC(req *JSONRpcReq) *ErrorReply {
	log.Printf("Unknown RPC method: %v", req)
	return &ErrorReply{Code: -1, Message: "Invalid method"}
}

func (s *StratumServer) broadcastNewJobs() {
	t := s.currentBlockTemplate()
	if t == nil || s.isSick() {
		return
	}
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	count := len(s.sessions)
	log.Printf("Broadcasting new jobs to %d miners", count)
	bcast := make(chan int, 1024*16)
	n := 0

	for m := range s.sessions {
		n++
		bcast <- n
		go func(cs *Session) {
			reply := cs.getJob(t)
			err := cs.pushMessage("job", &reply)
			fmt.Printf("[Job Broadcast] %+v\n", reply)
			<-bcast
			if err != nil {
				log.Printf("Job transmit error to %s: %v", cs.ip, err)
				s.removeSession(cs)
			} else {
				s.setDeadline(cs.conn)
			}
		}(m)
	}
}

func (s *StratumServer) refreshBlockTemplate(bcast bool) {
	newBlock := s.fetchBlockTemplate()
	if newBlock && bcast {
		s.broadcastNewJobs()
	}
}

// Optimized splitting functions with runes from @Peppinux (https://github.com/peppinux)
func (s *StratumServer) splitLoginString(loginWorkerPair string) (addr, wid, pid string, diff uint64) {
	currParam := paramAddr // String always starts with ADDRESS
	currSubstr := ""       // Substring starts empty

	// Since input vals from json are string, need to convert to a rune array, then references just use [0] slice since these are just '@', '+', '.' in config.json
	widAddrSep := []rune(s.config.Stratum.WorkerID.AddressSeparator)
	pidAddrSep := []rune(s.config.Stratum.PaymentID.AddressSeparator)
	fDiffAddrSep := []rune(s.config.Stratum.FixedDiff.AddressSeparator)

	lastPos := len(loginWorkerPair) - 1
	for pos, c := range loginWorkerPair {
		if c != widAddrSep[0] && c != pidAddrSep[0] && c != fDiffAddrSep[0] && pos != lastPos {
			currSubstr += string(c)
		} else {
			if pos == lastPos {
				currSubstr += string(c)
			}

			// Finalize substring
			switch currParam {
			case paramAddr:
				addr = currSubstr
			case paramWID:
				wid = currSubstr
			case paramPID:
				pid = currSubstr
			case paramDiff:
				diff, _ = strconv.ParseUint(currSubstr, 10, 64)
			}

			// Reset substring and find out next param type
			currSubstr = ""
			switch c {
			case widAddrSep[0]:
				currParam = paramWID
			case pidAddrSep[0]:
				currParam = paramPID
			case fDiffAddrSep[0]:
				currParam = paramDiff
			}
		}
	}
	return
}
