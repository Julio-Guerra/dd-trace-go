package tracer

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"math/rand"
	"time"

	"github.com/DataDog/sketches-go/ddsketch/encoding"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

type dataPipeline struct {
	pipelineHash uint64
	callTime time.Time
	service string
	pipelineName string
}

func dataPipelineFromBaggage(data []byte, service string) (DataPipeline, error) {
	pipeline := &dataPipeline{service: service}
	if len(data) < 8 {
		return nil, errors.New("pipeline hash smaller than 8 bytes")
	}
	pipeline.pipelineHash = binary.LittleEndian.Uint64(data)
	data = data[8:]
	t, err := encoding.DecodeVarint64(&data)
	if err != nil {
		return nil, err
	}
	pipeline.callTime = time.Unix(0, t*int64(time.Millisecond))
	return pipeline, nil
}

func (p *dataPipeline) ToBaggage() ([]byte, error) {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, p.pipelineHash)
	encoding.EncodeVarint64(&data, p.callTime.UnixNano()/int64(time.Millisecond))
	return data, nil
}

func (p *dataPipeline) GetCallTime() time.Time {
	return p.callTime
}

func (p *dataPipeline) GetHash() uint64 {
	return p.pipelineHash
}

// MergeWith merges passed data pipelines into the current one. It returns the current data pipeline.
func (p *dataPipeline) MergeWith(receivingPipelineName string, dataPipelines ...DataPipeline) (DataPipeline, error) {
	pipelines := make([]DataPipeline, 0, len(dataPipelines)+1)
	pipelines = append(pipelines, p.SetCheckpoint(receivingPipelineName))
	for _, d := range dataPipelines {
		pipelines = append(pipelines, d.SetCheckpoint(receivingPipelineName))
	}
	callTimes := make(map[uint64][]time.Time)
	for _, pipeline := range pipelines {
		callTimes[pipeline.GetHash()] = append(callTimes[pipeline.GetHash()], pipeline.GetCallTime())
	}
	hashes := make([]uint64, 0, len(callTimes))
	for h := range callTimes {
		hashes = append(hashes, h)
	}
	// randomly track one of the pipelines.
	// the hope is that with enough samples, we will track all the pipelines when fan-in happens.
	hash := hashes[rand.Intn(len(hashes))]
	callTime := callTimes[hash][rand.Intn(len(callTimes[hash]))]
	return &dataPipeline{pipelineHash: hash, service: p.service, callTime: callTime}, nil
}

func newDataPipeline(service string) *dataPipeline {
	now := time.Now()
	p := &dataPipeline{
		pipelineHash: 0,
		callTime: now,
		service: service,
	}
	return p.setCheckpoint("", now)
}

func nodeHash(service, receivingPipelineName string) uint64 {
	b := make([]byte, 0, len(service) + len(receivingPipelineName))
	b = append(b, service...)
	b = append(b, receivingPipelineName...)
	h := fnv.New64()
	h.Write(b)
	return h.Sum64()
}

func pipelineHash(nodeHash, parentHash uint64) uint64 {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b, nodeHash)
	binary.LittleEndian.PutUint64(b[8:], parentHash)
	h := fnv.New64()
	h.Write(b)
	return h.Sum64()
}

func (p *dataPipeline) SetCheckpoint(receivingPipelineName string) ddtrace.DataPipeline {
	return p.setCheckpoint(receivingPipelineName, time.Now())
}

func (p *dataPipeline) setCheckpoint(receivingPipelineName string, t time.Time) *dataPipeline {
	child := dataPipeline{
		pipelineHash: pipelineHash(nodeHash(p.service, receivingPipelineName), p.pipelineHash),
		callTime: p.callTime,
		service: p.service,
		pipelineName: receivingPipelineName,
	}
	if tracer, ok := internal.GetGlobalTracer().(*tracer); ok {
		select {
		case tracer.pipelineStats.In <- pipelineStatsPoint{
			service: p.service,
			receivingPipelineName: receivingPipelineName,
			parentHash: p.pipelineHash,
			pipelineHash: child.pipelineHash,
			timestamp: t.UnixNano(),
			latency: t.Sub(p.callTime).Nanoseconds(),
		}:
		default:
			log.Error("Pipeline stats channel full, disregarding stats point.")
		}
	}
	return &child
}