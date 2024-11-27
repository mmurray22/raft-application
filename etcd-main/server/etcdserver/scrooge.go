package etcdserver

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/dustin/go-humanize"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/pkg/v3/pbutil"
	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
	"go.etcd.io/etcd/server/v3/etcdserver/apply"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
	"go.etcd.io/etcd/server/v3/etcdserver/ipc-pkg"
	"go.etcd.io/etcd/server/v3/etcdserver/scrooge"
	"go.etcd.io/etcd/server/v3/storage/mvcc"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// "bytes"
// "encoding/gob"
// "encoding/json"
// "fmt"
// "log"
// "sync"

const (
	// pipes used by Scrooge
	path_to_ipipe      = "/tmp/scrooge-input"
	path_to_opipe      = "/tmp/scrooge-output"
	path_to_ccf_output = "/tmp/CCF.csv"
)

func (s *EtcdServer) CreatePipe() {
	err := ipc.CreatePipe(path_to_ipipe)
	if err != nil {
		fmt.Println("Unable to open output pipe: ", err)
	}
}

func (s *EtcdServer) ReadScrooge() {
	var err error

	// create read pipe
	err = ipc.CreatePipe(path_to_opipe)
	if err != nil {
		fmt.Printf("Unable to open output pipe: %v\n", err)
	} else {
		println("Successfully created output pipe", "\n")
	}

	// open pipe reader
	openReadPipe, err := ipc.OpenPipeReader(path_to_opipe)
	if err != nil {
		fmt.Println("Unable to open pipe reader: ", err)
	}
	defer openReadPipe.Close()
	reader := bufio.NewReader(openReadPipe)

	err = os.Remove(path_to_ccf_output)
	if err != nil {
		fmt.Printf("Unable to remove CCF output: %v", err)
	} else {
		fmt.Printf("Successfully removed CCF output")
	}

	_, err = os.Create(path_to_ccf_output)
	if err != nil {
		fmt.Println("Cannot create ccf file", err)
	}
	ccf_file, fileErr := os.OpenFile(path_to_ccf_output, os.O_WRONLY, 0777)
	if fileErr != nil {
		fmt.Println("Cannot open ccf file/C for writing:", fileErr)
	} else {
		fmt.Printf("Successfully opened CCF output for writing")
	}

	// open writer to the ccf output file
	ccf_output_writer := bufio.NewWriterSize(ccf_file, 32768)
	// continuously receives messages from Scrooge
	receiveScrooge(s, ccf_output_writer, reader)
}

func receiveScrooge(s *EtcdServer, ccf_output_writer *bufio.Writer, scrooge_output_pipe_reader *bufio.Reader) {
	var scroogeTransfer scrooge.ScroogeTransfer

	totalAppliedTxns := 0
	txnsTillNextDrPrint := 0

	for {
		pipeData, err := ipc.UsePipeReader(scrooge_output_pipe_reader)
		if err != nil {
			println("ERROR READING, MUST BREAK: ", err)
			os.Exit(1)
			break
		}
		err = proto.Unmarshal(pipeData, &scroogeTransfer)
		if err != nil {
			println("Error deserializing ScroogeTransfer")
		}

		switch transferType := scroogeTransfer.Transfer.(type) {
		case *scrooge.ScroogeTransfer_KeyValueHash:
			// Handle CCF usecase
			kvHash := scroogeTransfer.GetKeyValueHash()
			txn := s.kv.Read(mvcc.ConcurrentReadTxMode, traceutil.TODO())
			keyRange, err := txn.Range(context.TODO(), []byte(kvHash.Key), nil, mvcc.RangeOptions{Limit: 1})
			txn.End()
			println("READ:", hex.EncodeToString([]byte(kvHash.Key)), " :::: ", hex.EncodeToString([]byte(kvHash.ValueMd5Hash)))
			if err != nil {
				println("ERROR with reading key: '", kvHash.Key, "' when running CCF, err", err)
				continue
			}

			if keyRange.Count != 0 {
				md5hash := md5.Sum(keyRange.KVs[0].Value)
				localMd5HashString := hex.EncodeToString(md5hash[:])
				if localMd5HashString == kvHash.ValueMd5Hash {
					ccf_output_writer.WriteString(kvHash.Key + "," + kvHash.ValueMd5Hash + "," + localMd5HashString + ",AGREE\n")
				} else {
					ccf_output_writer.WriteString(kvHash.Key + "," + kvHash.ValueMd5Hash + "," + localMd5HashString + ",DISAGREE\n")
				}
			} else {
				ccf_output_writer.WriteString(kvHash.Key + "," + kvHash.ValueMd5Hash + ",,NO_VALUE\n")
			}

		case *scrooge.ScroogeTransfer_KeyValueUpdate:
			// legacyyyy
			println("Received unexpected key value update. Ignoring...")

		case *scrooge.ScroogeTransfer_CommitAcknowledgment:
			// commitAcknowledgment := scroogeTransfer.GetCommitAcknowledgment()
			// println("Received scrooge commit acknolwdgment ", commitAcknowledgment.SequenceNumber)

		case *scrooge.ScroogeTransfer_UnvalidatedCrossChainMessage:
			unvalidatedCrossChainMessage := scroogeTransfer.GetUnvalidatedCrossChainMessage()
			for _, crossChainData := range unvalidatedCrossChainMessage.Data {
				if crossChainData.SequenceNumber > 100 {
					applyTxn(s, crossChainData.MessageContent)
				}
			}
			totalAppliedTxns += len(unvalidatedCrossChainMessage.Data)
			txnsTillNextDrPrint -= len(unvalidatedCrossChainMessage.Data)
			if txnsTillNextDrPrint < 0 {
				txnsTillNextDrPrint += 50000
				println("Applied ", len(unvalidatedCrossChainMessage.Data), "transactions, in total: ", totalAppliedTxns, " txns applied")
			}

		default:
			println("Unknown Scrooge Transfer Type: ", transferType)
		}
	}
}

func applyTxn(s *EtcdServer, txn []byte) {
	shouldApplyV3 := membership.ApplyBoth
	var ar *apply.Result

	var raftReq pb.InternalRaftRequest
	if !pbutil.MaybeUnmarshal(&raftReq, txn) { // backward compatible
		var r pb.Request
		rp := &r
		pbutil.MustUnmarshal(rp, txn)
		s.lg.Debug("applyEntryNormal", zap.Stringer("V2request", rp))
		s.w.Trigger(r.ID, s.applyV2Request((*RequestV2)(rp), shouldApplyV3))
		return
	}
	s.lg.Debug("applyEntryNormal", zap.Stringer("raftReq", &raftReq))

	if raftReq.V2 != nil {
		req := (*RequestV2)(raftReq.V2)
		s.w.Trigger(req.ID, s.applyV2Request(req, shouldApplyV3))

		fmt.Println("Server finished applying V2Request!")
		return
	}

	id := raftReq.ID
	if id == 0 {
		if raftReq.Header == nil {
			s.lg.Panic("applyEntryNormal, could not find a header")
		}
		id = raftReq.Header.ID
	}

	needResult := s.w.IsRegistered(id)
	if needResult || !noSideEffect(&raftReq) {
		if !needResult && raftReq.Txn != nil {
			removeNeedlessRangeReqs(raftReq.Txn)
		}
		// raftReq.Put != nil -> write to pipe!!!
		ar = s.uberApply.Apply(&raftReq, shouldApplyV3)
	}

	// do not re-toApply applied entries.
	if !shouldApplyV3 {
		return
	}

	if ar == nil {
		return
	}

	if ar.Err != errors.ErrNoSpace || len(s.alarmStore.Get(pb.AlarmType_NOSPACE)) > 0 {
		s.w.Trigger(id, ar)
		return
	}

	lg := s.Logger()
	lg.Warn(
		"message exceeded backend quota; raising alarm",
		zap.Int64("quota-size-bytes", s.Cfg.QuotaBackendBytes),
		zap.String("quota-size", humanize.Bytes(uint64(s.Cfg.QuotaBackendBytes))),
		zap.Error(ar.Err),
	)

	s.GoAttach(func() {
		a := &pb.AlarmRequest{
			MemberID: uint64(s.MemberId()),
			Action:   pb.AlarmRequest_ACTIVATE,
			Alarm:    pb.AlarmType_NOSPACE,
		}
		s.raftRequest(s.ctx, pb.InternalRaftRequest{Alarm: a})
		s.w.Trigger(id, ar)
	})
}

func (s *EtcdServer) WriteScrooge() {
	var err error

	// var numEntries int = 0
	var sequenceNumber uint64 = 0

	// lg := s.Logger()

	// create write pipe
	err = ipc.CreatePipe(path_to_ipipe)
	if err != nil {
		fmt.Println("Unable to open input pipe: ", err)
	}

	// open pipe writer
	openWritePipe, err := ipc.OpenPipeWriter(path_to_ipipe)
	if err != nil {
		fmt.Println("Unable to open pipe writer: ", err)
	}

	// continously receives data of applied normal entries and subsequently writes the data to Scrooge
	writer := bufio.NewWriterSize(openWritePipe, 32768)
	for data := range s.WriteScroogeC {

		sendScrooge(data, sequenceNumber, writer) //openWritePipe)
		sequenceNumber++

		if sequenceNumber%100000 == 0 {
			fmt.Println("CURRENT SN: ", sequenceNumber)
		}
	}
}

func sendScrooge(payload []byte, seqNumber uint64, writer io.Writer) { // openWritePipe *os.File) {
	request := &scrooge.ScroogeRequest{
		Request: &scrooge.ScroogeRequest_SendMessageRequest{
			SendMessageRequest: &scrooge.SendMessageRequest{
				Content: &scrooge.CrossChainMessageData{
					MessageContent: payload,
					SequenceNumber: seqNumber,
				},
				ValidityProof: []byte("substitute valididty proof"),
			},
		},
	}
	// fmt.Println("Send Sequence Number: ", request.GetSendMessageRequest().GetContent().GetMessageContent())
	// fmt.Println("Send Payload: ", string(request.GetSendMessageRequest().GetContent().GetMessageContent()))

	var err error
	requestBytes, err := proto.Marshal(request)

	if err == nil {
		err = ipc.UsePipeWriter(writer, requestBytes)
		if err != nil {
			println("Unable to use pipe writer", err)
		}
	}
}
