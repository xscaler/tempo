package livestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/services"
	"github.com/grafana/tempo/pkg/ingest"
	"github.com/grafana/tempo/pkg/ingest/testkafka"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.uber.org/atomic"
)

const (
	testTopic         = "test-topic"
	testConsumerGroup = "test-consumer-group"
	testPartition     = int32(0)
)

func TestPartitionReaderCommits(t *testing.T) {
	t.Run("sync commits", func(t *testing.T) {
		k, address := testkafka.CreateCluster(t, 1, testTopic)

		kafkaCommits := atomic.NewInt32(0)
		k.ControlKey(kmsg.OffsetCommit, func(kmsg.Request) (kmsg.Response, error, bool) {
			kafkaCommits.Inc()
			return nil, nil, false
		})

		client := testkafka.NewKafkaClient(t, address, testTopic)
		testkafka.SendReq(t.Context(), t, client, ingest.Encode, testTenantID)

		consumeFn := func(_ context.Context, rs recordIter, _ time.Time) (*kadm.Offset, error) {
			var lastRecord *kgo.Record
			for !rs.Done() {
				lastRecord = rs.Next()
			}
			offset := kadm.NewOffsetFromRecord(lastRecord)
			return &offset, nil
		}

		// commitInterval=0 commits synchronously
		r := defaultPartitionReaderWithCommitInterval(t, address, 0, consumeFn)

		assert.Eventually(t, func() bool { return kafkaCommits.Load() >= 1 }, time.Second*2, 10*time.Millisecond)
		assert.Equal(t, int64(0), r.lag.Load())

		t.Cleanup(func() { require.NoError(t, services.StopAndAwaitTerminated(context.Background(), r)) })
	})

	t.Run("async commits", func(t *testing.T) {
		var asyncCommits atomic.Int32

		commitInterval := 5 * time.Second

		k, address := testkafka.CreateCluster(t, 1, testTopic)
		k.ControlKey(kmsg.OffsetCommit, func(kmsg.Request) (kmsg.Response, error, bool) {
			asyncCommits.Inc()
			return nil, nil, false
		})

		client := testkafka.NewKafkaClient(t, address, testTopic)
		testkafka.SendReq(t.Context(), t, client, ingest.Encode, testTenantID)

		consumed := make(chan struct{})
		consumeFn := func(_ context.Context, rs recordIter, _ time.Time) (*kadm.Offset, error) {
			defer close(consumed)
			var lastRecord *kgo.Record
			for !rs.Done() {
				lastRecord = rs.Next()
			}
			offset := kadm.NewOffsetFromRecord(lastRecord)
			return &offset, nil
		}

		r := defaultPartitionReaderWithCommitInterval(t, address, commitInterval, consumeFn)

		<-consumed                                     // Assert that record has been consumed
		assert.Equal(t, asyncCommits.Load(), int32(0)) // Nothing committed

		// Waiting up to commitInterval, a commit will have happened by then
		assert.Eventually(t, func() bool { return asyncCommits.Load() >= 1 }, commitInterval*2, 10*time.Millisecond)
		assert.Equal(t, int64(0), r.lag.Load())

		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), r))
	})
}

func TestPartitionReaderLag(t *testing.T) {
	k, address := testkafka.CreateCluster(t, 1, testTopic)

	kafkaCommits := atomic.NewInt32(0)
	k.ControlKey(kmsg.OffsetCommit, func(kmsg.Request) (kmsg.Response, error, bool) {
		kafkaCommits.Inc()
		return nil, nil, false
	})

	var counter int
	consumeFn := func(_ context.Context, rs recordIter, _ time.Time) (*kadm.Offset, error) {
		counter++
		if counter <= 1 { // allow to process one record to store lag
			lastRecord := rs.Next()
			offset := kadm.NewOffsetFromRecord(lastRecord)
			return &offset, nil
		}
		return nil, errors.New("error consuming records")
	}

	// commitInterval=0 commits synchronously
	r := defaultPartitionReaderWithCommitInterval(t, address, 0, consumeFn)

	client := testkafka.NewKafkaClient(t, address, testTopic)
	records := 10
	for range records {
		testkafka.SendReq(t.Context(), t, client, ingest.Encode, testTenantID)
	}

	time.Sleep(time.Second)
	assert.Equal(t, int32(1), kafkaCommits.Load(), "only one record should be committed")
	assert.Equal(t, int64(records)-1, r.lag.Load(), "only one record should be committed")

	t.Cleanup(func() { require.NoError(t, services.StopAndAwaitTerminated(context.Background(), r)) })
}

func TestFetchLastCommittedOffsetForceFromLookback(t *testing.T) {
	lookback := time.Hour

	t.Run("committed offset exists, forceFromLookback=false uses committed offset", func(t *testing.T) {
		_, address := testkafka.CreateCluster(t, 1, testTopic)
		client := testkafka.NewKafkaClient(t, address, testTopic)

		// Produce a record and commit an offset
		testkafka.SendReq(t.Context(), t, client, ingest.Encode, testTenantID)

		adm := kadm.NewClient(client)
		offsets := make(kadm.Offsets)
		offsets.Add(kadm.Offset{
			Topic:     testTopic,
			Partition: testPartition,
			At:        0,
		})
		_, err := adm.CommitOffsets(t.Context(), testConsumerGroup, offsets)
		require.NoError(t, err)

		l := test.NewTestingLogger(t)
		cfg := ingest.KafkaConfig{}
		flagext.DefaultValues(&cfg)
		cfg.Address = address
		cfg.Topic = testTopic
		cfg.ConsumerGroup = testConsumerGroup

		readerClient, err := ingest.NewReaderClient(cfg, ingest.NewReaderClientMetrics(liveStoreServiceName, prometheus.NewRegistry()), l)
		require.NoError(t, err)

		r, err := newPartitionReader(readerClient, testPartition, cfg, 0, lookback, false, nil, l, newPartitionReaderMetrics(testPartition, prometheus.NewRegistry()))
		require.NoError(t, err)

		offset, err := r.fetchLastCommittedOffset(t.Context())
		require.NoError(t, err)

		// Should use committed offset (At(0)), not lookback
		epochOffset := offset.EpochOffset()
		assert.Equal(t, int64(0), epochOffset.Offset)
	})

	t.Run("committed offset exists, forceFromLookback=true ignores committed offset", func(t *testing.T) {
		_, address := testkafka.CreateCluster(t, 1, testTopic)
		client := testkafka.NewKafkaClient(t, address, testTopic)

		// Produce a record and commit an offset
		testkafka.SendReq(t.Context(), t, client, ingest.Encode, testTenantID)

		adm := kadm.NewClient(client)
		offsets := make(kadm.Offsets)
		offsets.Add(kadm.Offset{
			Topic:     testTopic,
			Partition: testPartition,
			At:        0,
		})
		_, err := adm.CommitOffsets(t.Context(), testConsumerGroup, offsets)
		require.NoError(t, err)

		l := test.NewTestingLogger(t)
		cfg := ingest.KafkaConfig{}
		flagext.DefaultValues(&cfg)
		cfg.Address = address
		cfg.Topic = testTopic
		cfg.ConsumerGroup = testConsumerGroup

		readerClient, err := ingest.NewReaderClient(cfg, ingest.NewReaderClientMetrics(liveStoreServiceName, prometheus.NewRegistry()), l)
		require.NoError(t, err)

		r, err := newPartitionReader(readerClient, testPartition, cfg, 0, lookback, true, nil, l, newPartitionReaderMetrics(testPartition, prometheus.NewRegistry()))
		require.NoError(t, err)

		offset, err := r.fetchLastCommittedOffset(t.Context())
		require.NoError(t, err)

		// Should start at the first record inside the lookback period (offset 0),
		// resolved to an exact offset rather than handed to kgo as AfterMilli.
		assert.Equal(t, int64(0), offset.EpochOffset().Offset)
	})

	t.Run("forceFromLookback=true with no record inside the lookback starts at the end", func(t *testing.T) {
		_, address := testkafka.CreateCluster(t, 1, testTopic)
		client := testkafka.NewKafkaClient(t, address, testTopic)

		// Two records, committed past both.
		testkafka.SendReq(t.Context(), t, client, ingest.Encode, testTenantID)
		testkafka.SendReq(t.Context(), t, client, ingest.Encode, testTenantID)
		offsets := make(kadm.Offsets)
		offsets.Add(kadm.Offset{Topic: testTopic, Partition: testPartition, At: 2})
		_, err := kadm.NewClient(client).CommitOffsets(t.Context(), testConsumerGroup, offsets)
		require.NoError(t, err)

		l := test.NewTestingLogger(t)
		cfg := ingest.KafkaConfig{}
		flagext.DefaultValues(&cfg)
		cfg.Address = address
		cfg.Topic = testTopic
		cfg.ConsumerGroup = testConsumerGroup

		readerClient, err := ingest.NewReaderClient(cfg, ingest.NewReaderClientMetrics(liveStoreServiceName, prometheus.NewRegistry()), l)
		require.NoError(t, err)

		// A lookback that starts in the future excludes every record: start at the end.
		r, err := newPartitionReader(readerClient, testPartition, cfg, 0, -time.Hour, true, nil, l, newPartitionReaderMetrics(testPartition, prometheus.NewRegistry()))
		require.NoError(t, err)

		offset, err := r.fetchLastCommittedOffset(t.Context())
		require.NoError(t, err)
		assert.Equal(t, int64(2), offset.EpochOffset().Offset)
	})

	t.Run("no committed offset, forceFromLookback=true uses lookback period", func(t *testing.T) {
		_, address := testkafka.CreateCluster(t, 1, testTopic)

		l := test.NewTestingLogger(t)
		cfg := ingest.KafkaConfig{}
		flagext.DefaultValues(&cfg)
		cfg.Address = address
		cfg.Topic = testTopic
		cfg.ConsumerGroup = testConsumerGroup

		readerClient, err := ingest.NewReaderClient(cfg, ingest.NewReaderClientMetrics(liveStoreServiceName, prometheus.NewRegistry()), l)
		require.NoError(t, err)

		r, err := newPartitionReader(readerClient, testPartition, cfg, 0, lookback, true, nil, l, newPartitionReaderMetrics(testPartition, prometheus.NewRegistry()))
		require.NoError(t, err)

		offset, err := r.fetchLastCommittedOffset(t.Context())
		require.NoError(t, err)

		// Empty partition: the lookback resolves to the end offset, 0.
		assert.Equal(t, int64(0), offset.EpochOffset().Offset)
	})
}

// fakeOffsetLister answers timestamp offset lookups without a broker.
type fakeOffsetLister struct {
	offsets kadm.ListedOffsets
	err     error
	calls   atomic.Int32
}

func (f *fakeOffsetLister) ListOffsetsAfterMilli(context.Context, int64, ...string) (kadm.ListedOffsets, error) {
	f.calls.Inc()
	return f.offsets, f.err
}

func listedOffset(offset int64, err error) kadm.ListedOffsets {
	return kadm.ListedOffsets{testTopic: {testPartition: {Topic: testTopic, Partition: testPartition, Offset: offset, Err: err}}}
}

// answerTimestampLookupsWithMinusOne makes the cluster behave like an AutoMQ
// zone proxy broker (automq_az client IDs): ListOffsets by timestamp returns
// offset -1, which kgo reads as "nothing after this time" and so starts at the end.
func answerTimestampLookupsWithMinusOne(k *testkafka.Cluster) {
	k.ControlKey(kmsg.ListOffsets, func(req kmsg.Request) (kmsg.Response, error, bool) {
		k.KeepControl()
		r := req.(*kmsg.ListOffsetsRequest)
		resp := r.ResponseKind().(*kmsg.ListOffsetsResponse)
		for _, rt := range r.Topics {
			st := kmsg.NewListOffsetsResponseTopic()
			st.Topic = rt.Topic
			for _, rp := range rt.Partitions {
				if rp.Timestamp < 0 { // -1 latest, -2 earliest: let kfake answer
					return nil, nil, false
				}
				sp := kmsg.NewListOffsetsResponseTopicPartition()
				sp.Partition = rp.Partition
				sp.Offset = -1
				sp.Timestamp = -1
				sp.LeaderEpoch = -1
				st.Partitions = append(st.Partitions, sp)
			}
			resp.Topics = append(resp.Topics, st)
		}
		return resp, nil, true
	})
}

func TestFetchLastCommittedOffsetLookbackLister(t *testing.T) {
	newReader := func(t *testing.T, lister offsetLister) *PartitionReader {
		_, address := testkafka.CreateCluster(t, 1, testTopic)
		l := test.NewTestingLogger(t)
		cfg := ingest.KafkaConfig{}
		flagext.DefaultValues(&cfg)
		cfg.Address = address
		cfg.Topic = testTopic
		cfg.ConsumerGroup = testConsumerGroup

		client, err := ingest.NewReaderClient(cfg, ingest.NewReaderClientMetrics(liveStoreServiceName, prometheus.NewRegistry()), l)
		require.NoError(t, err)
		t.Cleanup(client.Close)

		r, err := newPartitionReader(client, testPartition, cfg, 0, time.Hour, true, nil, l, newPartitionReaderMetrics(testPartition, prometheus.NewRegistry()))
		require.NoError(t, err)
		r.lookbackLister = lister
		return r
	}

	tests := []struct {
		name          string
		lister        *fakeOffsetLister
		wantOffset    int64
		wantAfterMill bool // deferred to kgo: Offset carries the timestamp, epoch -1
		wantErr       bool
	}{
		{name: "starts at the offset the lister returns", lister: &fakeOffsetLister{offsets: listedOffset(4, nil)}, wantOffset: 4},
		{name: "starts at offset 0", lister: &fakeOffsetLister{offsets: listedOffset(0, nil)}, wantOffset: 0},
		{name: "lister error", lister: &fakeOffsetLister{err: errors.New("broker down")}, wantErr: true},
		{name: "partition error", lister: &fakeOffsetLister{offsets: listedOffset(0, kerr.NotLeaderForPartition)}, wantErr: true},
		{name: "partition missing defers to kgo", lister: &fakeOffsetLister{offsets: kadm.ListedOffsets{}}, wantAfterMill: true},
		{name: "unknown partition defers to kgo", lister: &fakeOffsetLister{offsets: listedOffset(-1, kerr.UnknownTopicOrPartition)}, wantAfterMill: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReader(t, tt.lister)

			offset, err := r.fetchLastCommittedOffset(t.Context())
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, int32(1), tt.lister.calls.Load())
			if tt.wantAfterMill {
				assert.InDelta(t, time.Now().Add(-time.Hour).UnixMilli(), offset.EpochOffset().Offset, 5000)
				return
			}
			// An exact offset, not AfterMilli (which would carry the timestamp in Offset).
			assert.Equal(t, tt.wantOffset, offset.EpochOffset().Offset)
		})
	}
}

// TestPartitionReaderLookbackReplayWithZoneProxyBroker reproduces the AutoMQ
// zone-routing bug end to end: the reader's own client gets -1 for the
// lookback timestamp, so without a separate lookup it starts at the end and
// replays nothing. With a lookup through a client the broker answers
// correctly, every record in the lookback is replayed.
func TestPartitionReaderLookbackReplayWithZoneProxyBroker(t *testing.T) {
	const records = 3

	k, address := testkafka.CreateCluster(t, 1, testTopic)
	producer := testkafka.NewKafkaClient(t, address, testTopic)
	for range records {
		testkafka.SendReq(t.Context(), t, producer, ingest.Encode, testTenantID)
	}

	// The correct answer, read before the broker starts misbehaving: the
	// first offset after the lookback start is the first record.
	correct, err := kadm.NewClient(producer).ListOffsetsAfterMilli(t.Context(), time.Now().Add(-time.Hour).UnixMilli(), testTopic)
	require.NoError(t, err)

	answerTimestampLookupsWithMinusOne(k)

	l := test.NewTestingLogger(t)
	cfg := ingest.KafkaConfig{}
	flagext.DefaultValues(&cfg)
	cfg.Address = address
	cfg.Topic = testTopic
	cfg.ConsumerGroup = testConsumerGroup

	run := func(t *testing.T, lister offsetLister) int32 {
		var consumed atomic.Int32
		client, err := ingest.NewReaderClient(cfg, ingest.NewReaderClientMetrics(liveStoreServiceName, prometheus.NewRegistry()), l)
		require.NoError(t, err)
		t.Cleanup(client.Close)

		r, err := newPartitionReader(client, testPartition, cfg, 0, time.Hour, true, func(_ context.Context, rs recordIter, _ time.Time) (*kadm.Offset, error) {
			var last *kgo.Record
			for !rs.Done() {
				last = rs.Next()
				consumed.Inc()
			}
			o := kadm.NewOffsetFromRecord(last)
			return &o, nil
		}, l, newPartitionReaderMetrics(testPartition, prometheus.NewRegistry()))
		require.NoError(t, err)
		if lister != nil {
			r.lookbackLister = lister
		}
		require.NoError(t, services.StartAndAwaitRunning(t.Context(), r))
		time.Sleep(2 * time.Second)
		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), r))
		return consumed.Load()
	}

	t.Run("lookup through the reader's own client replays nothing", func(t *testing.T) {
		assert.Equal(t, int32(0), run(t, nil))
	})
	t.Run("lookup through a separate client replays the lookback", func(t *testing.T) {
		assert.Equal(t, int32(records), run(t, &fakeOffsetLister{offsets: correct}))
	})
}

func defaultPartitionReaderWithCommitInterval(t *testing.T, address string, commitInterval time.Duration, consume consumeFn) *PartitionReader {
	l := test.NewTestingLogger(t)

	cfg := ingest.KafkaConfig{}
	flagext.DefaultValues(&cfg)
	cfg.Address = address
	cfg.Topic = testTopic
	cfg.ConsumerGroup = testConsumerGroup

	client, err := ingest.NewReaderClient(
		cfg,
		ingest.NewReaderClientMetrics(liveStoreServiceName, prometheus.NewRegistry()),
		l,
	)
	require.NoError(t, err)

	r, err := newPartitionReader(client, 0, cfg, commitInterval, time.Hour, false, consume, l, newPartitionReaderMetrics(testPartition, prometheus.NewRegistry()))
	require.NoError(t, err)

	err = services.StartAndAwaitRunning(t.Context(), r)
	require.NoError(t, err)

	return r
}
