/**
 * Copyright 2016-2023 Confluent Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kafka

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"path"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go/modules/compose"
)

// producer test control
type producerCtrl struct {
	silent        bool
	withDr        bool // use delivery channel
	batchProducer bool // enable batch producer
}

// define commitMode with constants
type commitMode string

const (
	ViaCommitMessageAPI = "CommitMessage"
	ViaCommitOffsetsAPI = "CommitOffsets"
	ViaCommitAPI        = "Commit"
)

// consumer test control
type consumerCtrl struct {
	autoCommit bool // set enable.auto.commit property
	useChannel bool
	commitMode commitMode // which commit api to use
}

type testmsgType struct {
	msg           Message
	expectedError Error
}

// msgtracker tracks messages
type msgtracker struct {
	t      *testing.T
	msgcnt int64
	errcnt int64 // count of failed messages
	msgs   []*Message
}

// msgtrackerStart sets up a new message tracker
func msgtrackerStart(t *testing.T, expectedCnt int) (mt msgtracker) {
	mt = msgtracker{t: t}
	mt.msgs = make([]*Message, expectedCnt)
	return mt
}

// findConsumerGroupListings returns the ConsumerGroupListing for a group with name `group`
// from a slice of ConsumerGroupListings, and nil otherwise.
func findConsumerGroupListing(groups []ConsumerGroupListing, group string) *ConsumerGroupListing {
	for _, groupInfo := range groups {
		if groupInfo.GroupID == group {
			return &groupInfo
		}
	}
	return nil
}

// findConsumerGroupListings returns the ConsumerGroupDescription for a group with name `group`
// from a slice of ConsumerGroupDescription, and nil otherwise.
func findConsumerGroupDescription(groups []ConsumerGroupDescription, group string) *ConsumerGroupDescription {
	for _, groupInfo := range groups {
		if groupInfo.GroupID == group {
			return &groupInfo
		}
	}
	return nil
}

// checkGroupDesc is a helper function to check the validity of a ConsumerGroupDescription.
// We can't directly use DeepEqual because some fields/slice orders change with every run.
func checkGroupDesc(
	groupDesc *ConsumerGroupDescription, state ConsumerGroupState, group string,
	protocol string, clientIDToPartitions map[string][]TopicPartition) bool {
	if groupDesc.GroupID != group ||
		groupDesc.State != state ||
		groupDesc.Error.Code() != ErrNoError ||
		groupDesc.PartitionAssignor != protocol ||
		// We can't check exactly the Broker information, but we add a check for the zero-value of the Host.
		groupDesc.Coordinator.Host == "" ||
		// We will run all our tests on non-simple consumer groups only.
		groupDesc.IsSimpleConsumerGroup ||
		len(groupDesc.Members) != len(clientIDToPartitions) ||
		// We don't set IncludeAuthorizedOperations while using helper.
		len(groupDesc.AuthorizedOperations) > 0 {
		return false
	}

	for _, member := range groupDesc.Members {
		if partitions, ok := clientIDToPartitions[member.ClientID]; !ok ||
			!reflect.DeepEqual(partitions, member.Assignment.TopicPartitions) {
			return false
		}
	}

	return true
}

var testMsgsInit = false
var p0TestMsgs []*testmsgType // partition 0 test messages
// pAllTestMsgs holds messages for various partitions including PartitionAny and  invalid partitions
var pAllTestMsgs []*testmsgType

// createTestMessages populates p0TestMsgs and pAllTestMsgs
func createTestMessages() {

	if testMsgsInit {
		return
	}
	defer func() { testMsgsInit = true }()

	testmsgs := make([]*testmsgType, 100)
	i := 0

	// a test message with default initialization
	testmsgs[i] = &testmsgType{msg: Message{TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: 0}}}
	i++

	// a test message for partition 0 with only Opaque specified
	testmsgs[i] = &testmsgType{msg: Message{TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: 0},
		Opaque: fmt.Sprintf("Op%d", i),
	}}
	i++

	// a test message for partition 0 with empty Value and Keys
	testmsgs[i] = &testmsgType{msg: Message{TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: 0},
		Value:  []byte(""),
		Key:    []byte(""),
		Opaque: fmt.Sprintf("Op%d", i),
	}}
	i++

	// a test message for partition 0 with Value, Key, and Opaque
	testmsgs[i] = &testmsgType{msg: Message{TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: 0},
		Value:  []byte(fmt.Sprintf("value%d", i)),
		Key:    []byte(fmt.Sprintf("key%d", i)),
		Opaque: fmt.Sprintf("Op%d", i),
	}}
	i++

	// a test message for partition 0 without  Value
	testmsgs[i] = &testmsgType{msg: Message{TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: 0},
		Key:    []byte(fmt.Sprintf("key%d", i)),
		Opaque: fmt.Sprintf("Op%d", i),
	}}
	i++

	// a test message for partition 0 without Key
	testmsgs[i] = &testmsgType{msg: Message{TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: 0},
		Value:  []byte(fmt.Sprintf("value%d", i)),
		Opaque: fmt.Sprintf("Op%d", i),
	}}
	i++

	p0TestMsgs = testmsgs[:i]

	// a test message for PartitonAny with Value, Key, and Opaque
	testmsgs[i] = &testmsgType{msg: Message{TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: PartitionAny},
		Value:  []byte(fmt.Sprintf("value%d", i)),
		Key:    []byte(fmt.Sprintf("key%d", i)),
		Opaque: fmt.Sprintf("Op%d", i),
	}}
	i++

	// a test message for a non-existent partition with Value, Key, and Opaque.
	// It should generate ErrUnknownPartition
	testmsgs[i] = &testmsgType{expectedError: Error{code: ErrUnknownPartition},
		msg: Message{TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: int32(10000)},
			Value:  []byte(fmt.Sprintf("value%d", i)),
			Key:    []byte(fmt.Sprintf("key%d", i)),
			Opaque: fmt.Sprintf("Op%d", i),
		}}
	i++

	pAllTestMsgs = testmsgs[:i]
}

// consume messages through the Poll() interface
func eventTestPollConsumer(c *Consumer, mt *msgtracker, expCnt int) {
	for true {
		ev := c.Poll(100)
		if ev == nil {
			// timeout
			continue
		}
		if !handleTestEvent(c, mt, expCnt, ev) {
			break
		}
	}
}

// consume messages through the Events channel
func eventTestChannelConsumer(c *Consumer, mt *msgtracker, expCnt int) {
	for ev := range c.Events() {
		if !handleTestEvent(c, mt, expCnt, ev) {
			break
		}
	}
}

// handleTestEvent returns false if processing should stop, else true. Tracks the message received
func handleTestEvent(c *Consumer, mt *msgtracker, expCnt int, ev Event) bool {
	switch e := ev.(type) {
	case *Message:
		if e.TopicPartition.Error != nil {
			mt.t.Errorf("Error: %v", e.TopicPartition)
		}
		mt.msgs[mt.msgcnt] = e
		mt.msgcnt++
		if mt.msgcnt >= int64(expCnt) {
			return false
		}
	case PartitionEOF:
		break // silence
	default:
		mt.t.Fatalf("Consumer error: %v", e)
	}
	return true

}

// delivery event handler. Tracks the message received
func deliveryTestHandler(t *testing.T, expCnt int64, deliveryChan chan Event, mt *msgtracker, doneChan chan int64) {

	for ev := range deliveryChan {
		m, ok := ev.(*Message)
		if !ok {
			continue
		}

		mt.msgs[mt.msgcnt] = m
		mt.msgcnt++

		if m.TopicPartition.Error != nil {
			mt.errcnt++
			// log it and check it later
			t.Logf("Message delivery error: %v", m.TopicPartition)
		}

		t.Logf("Delivered %d/%d to %s, error count %d", mt.msgcnt, expCnt, m.TopicPartition, mt.errcnt)

		if mt.msgcnt >= expCnt {
			break
		}

	}

	doneChan <- mt.msgcnt
	close(doneChan)
}

// producerTest produces messages in <testmsgs> to topic. Verifies delivered messages
func producerTest(t *testing.T, testname string, testmsgs []*testmsgType, pc producerCtrl, produceFunc func(p *Producer, m *Message, drChan chan Event)) {

	if testmsgs == nil {
		createTestMessages()
		testmsgs = pAllTestMsgs
	}

	//get the number of messages prior to producing more messages
	prerunMsgCnt, err := getMessageCountInTopic(testconf.TopicName)
	if err != nil {
		t.Fatalf("Cannot get message count, Error: %s\n", err)
	}

	conf := ConfigMap{"bootstrap.servers": testconf.Brokers,
		"go.batch.producer":            pc.batchProducer,
		"go.delivery.reports":          pc.withDr,
		"queue.buffering.max.messages": len(testmsgs),
		"acks":                         "all"}

	conf.updateFromTestconf()

	p, err := NewProducer(&conf)
	if err != nil {
		panic(err)
	}

	mt := msgtrackerStart(t, len(testmsgs))

	var doneChan chan int64
	var drChan chan Event

	if pc.withDr {
		doneChan = make(chan int64)
		drChan = p.Events()
		go deliveryTestHandler(t, int64(len(testmsgs)), p.Events(), &mt, doneChan)
	}

	if !pc.silent {
		t.Logf("%s: produce %d messages", testname, len(testmsgs))
	}

	for i := 0; i < len(testmsgs); i++ {
		t.Logf("producing message %d: %v\n", i, testmsgs[i].msg)
		produceFunc(p, &testmsgs[i].msg, drChan)
	}

	if !pc.silent {
		t.Logf("produce done")
	}

	// Wait for messages in-flight and in-queue to get delivered.
	if !pc.silent {
		t.Logf("%s: %d messages in queue", testname, p.Len())
	}

	r := p.Flush(10000)
	if r > 0 {
		t.Errorf("%s: %d messages remains in queue after Flush()", testname, r)
	}

	if pc.withDr {
		mt.msgcnt = <-doneChan
	} else {
		mt.msgcnt = int64(len(testmsgs))
	}

	if !pc.silent {
		t.Logf("delivered %d messages\n", mt.msgcnt)
	}

	p.Close()

	//get the number of messages afterward
	postrunMsgCnt, err := getMessageCountInTopic(testconf.TopicName)
	if err != nil {
		t.Fatalf("Cannot get message count, Error: %s\n", err)
	}

	if !pc.silent {
		t.Logf("prerun message count: %d,  postrun count %d, delta: %d\n", prerunMsgCnt, postrunMsgCnt, postrunMsgCnt-prerunMsgCnt)
		t.Logf("deliveried message count: %d,  error message count %d\n", mt.msgcnt, mt.errcnt)

	}

	// verify the count and messages only if we get the delivered messages
	if pc.withDr {
		if int64(postrunMsgCnt-prerunMsgCnt) != (mt.msgcnt - mt.errcnt) {
			t.Errorf("Expected topic message count %d, got %d\n", prerunMsgCnt+int(mt.msgcnt-mt.errcnt), postrunMsgCnt)
		}

		verifyMessages(t, mt.msgs, testmsgs)
	}
}

// consumerTest consumes messages from a pre-primed (produced to) topic.
// assignmentStrategy may be "" to use the default strategy.
func consumerTest(t *testing.T, testname string, assignmentStrategy string, msgcnt int, cc consumerCtrl, consumeFunc func(c *Consumer, mt *msgtracker, expCnt int), rebalanceCb func(c *Consumer, event Event) error) {

	if msgcnt == 0 {
		createTestMessages()
		producerTest(t, "Priming producer", p0TestMsgs, producerCtrl{},
			func(p *Producer, m *Message, drChan chan Event) {
				p.ProduceChannel() <- m
			})
		msgcnt = len(p0TestMsgs)
	}

	conf := ConfigMap{"bootstrap.servers": testconf.Brokers,
		"go.events.channel.enable": cc.useChannel,
		"group.id": testconf.GroupID +
			fmt.Sprintf("-%d", rand.Intn(1000000)),
		"session.timeout.ms":  6000,
		"api.version.request": "true",
		"enable.auto.commit":  cc.autoCommit,
		"debug":               ",",
		"auto.offset.reset":   "earliest"}
	if assignmentStrategy != "" {
		conf["partition.assignment.strategy"] = assignmentStrategy
	}

	conf.updateFromTestconf()

	c, err := testNewConsumer(t, &conf)

	if err != nil {
		panic(err)
	}
	defer c.Close()

	expCnt := msgcnt
	mt := msgtrackerStart(t, expCnt)

	t.Logf("%s, expecting %d messages", testname, expCnt)
	c.Subscribe(testconf.TopicName, rebalanceCb)

	consumeFunc(c, &mt, expCnt)

	//test commits
	switch cc.commitMode {
	case ViaCommitMessageAPI:
		// verify CommitMessage() API
		for _, message := range mt.msgs {
			_, commitErr := c.CommitMessage(message)
			if commitErr != nil {
				t.Errorf("Cannot commit message. Error: %s\n", commitErr)
			}
		}
	case ViaCommitOffsetsAPI:
		// verify CommitOffset
		partitions := make([]TopicPartition, len(mt.msgs))
		for index, message := range mt.msgs {
			partitions[index] = message.TopicPartition
		}
		_, commitErr := c.CommitOffsets(partitions)
		if commitErr != nil {
			t.Errorf("Failed to commit using CommitOffsets. Error: %s\n", commitErr)
		}
	case ViaCommitAPI:
		// verify Commit() API
		_, commitErr := c.Commit()
		if commitErr != nil {
			t.Errorf("Failed to commit. Error: %s", commitErr)
		}

	}

	// Trigger RevokePartitions
	c.Unsubscribe()

	// Handle RevokePartitions
	c.Poll(500)

}

// use opaque string to locate the matching test message for message verification
func findExpectedMessage(expected []*testmsgType, opaque string) *testmsgType {
	for i, m := range expected {
		if expected[i].msg.Opaque != nil && expected[i].msg.Opaque.(string) == opaque {
			return m
		}
	}
	return nil
}

// verify the message content against the expected
func verifyMessages(t *testing.T, msgs []*Message, expected []*testmsgType) {
	if len(msgs) != len(expected) {
		t.Errorf("Expected %d messages, got %d instead\n", len(expected), len(msgs))
		return
	}
	for _, m := range msgs {
		if m.Opaque == nil {
			continue // No way to look up the corresponding expected message, let it go
		}
		testmsg := findExpectedMessage(expected, m.Opaque.(string))
		if testmsg == nil {
			t.Errorf("Cannot find a matching expected message for message %v\n", m)
			continue
		}
		em := testmsg.msg
		if m.TopicPartition.Error != nil {
			if m.TopicPartition.Error != testmsg.expectedError {
				t.Errorf("Expected error %s, but got error %s\n", testmsg.expectedError, m.TopicPartition.Error)
			}
			continue
		}

		// check partition
		if em.TopicPartition.Partition == PartitionAny {
			if m.TopicPartition.Partition < 0 {
				t.Errorf("Expected partition %d, got %d\n", em.TopicPartition.Partition, m.TopicPartition.Partition)
			}
		} else if em.TopicPartition.Partition != m.TopicPartition.Partition {
			t.Errorf("Expected partition %d, got %d\n", em.TopicPartition.Partition, m.TopicPartition.Partition)
		}

		//check Key, Value, and Opaque
		if string(m.Key) != string(em.Key) {
			t.Errorf("Expected Key %v, got %v\n", m.Key, em.Key)
		}
		if string(m.Value) != string(em.Value) {
			t.Errorf("Expected Value %v, got %v\n", m.Value, em.Value)
		}
		if m.Opaque.(string) != em.Opaque.(string) {
			t.Errorf("Expected Opaque %v, got %v\n", m.Opaque, em.Opaque)
		}

	}
}

// test consumer APIs with various message commit modes
func consumerTestWithCommits(t *testing.T, testname string, assignmentStrategy string, msgcnt int, useChannel bool, consumeFunc func(c *Consumer, mt *msgtracker, expCnt int), rebalanceCb func(c *Consumer, event Event) error) {

	t.Logf("FIXME: Skipping auto commit test, it seems the Unsubscribe operation" +
		"doesn't complete the auto commit, while the Close operation does it\n")
	// consumerTest(t, testname+" auto commit", assignmentStrategy,
	// 	msgcnt, consumerCtrl{useChannel: useChannel, autoCommit: true}, consumeFunc, rebalanceCb)

	consumerTest(t, testname+" using CommitMessage() API", assignmentStrategy,
		msgcnt, consumerCtrl{useChannel: useChannel, commitMode: ViaCommitMessageAPI}, consumeFunc, rebalanceCb)

	consumerTest(t, testname+" using CommitOffsets() API", assignmentStrategy,
		msgcnt, consumerCtrl{useChannel: useChannel, commitMode: ViaCommitOffsetsAPI}, consumeFunc, rebalanceCb)

	consumerTest(t, testname+" using Commit() API", assignmentStrategy,

		msgcnt, consumerCtrl{useChannel: useChannel, commitMode: ViaCommitAPI}, consumeFunc, rebalanceCb)

}

func validateTopicResult(t *testing.T, result []TopicResult, expError map[string]Error) {
	for _, res := range result {
		exp, ok := expError[res.Topic]
		if !ok {
			t.Errorf("Result for unexpected topic %s", res)
			continue
		}

		if res.Error.Code() != exp.Code() {
			t.Errorf("Topic %s: expected \"%s\", got \"%s\"",
				res.Topic, exp, res.Error)
			continue
		}

		t.Logf("Topic %s: matched expected \"%s\"", res.Topic, res.Error)
	}
}

func validateConfig(t *testing.T, results []ConfigResourceResult, expResults []ConfigResourceResult, checkConfigEntries bool) {

	_, file, line, _ := runtime.Caller(1)
	caller := fmt.Sprintf("%s:%d", path.Base(file), line)

	if len(results) != len(expResults) {
		t.Fatalf("%s: Expected %d results, got %d: %v", caller, len(expResults), len(results), results)
	}

	for i, result := range results {
		expResult := expResults[i]

		if result.Error.Code() != expResult.Error.Code() {
			t.Errorf("%s: %v: Expected %v, got %v", caller, result, expResult.Error.Code(), result.Error.Code())
			continue
		}

		if !checkConfigEntries {
			continue
		}

		matchCnt := 0
		for _, expEntry := range expResult.Config {

			entry, ok := result.Config[expEntry.Name]
			if !ok {
				t.Errorf("%s: %v: expected config %s not found in result", caller, result, expEntry.Name)
				continue
			}

			if entry.Value != expEntry.Value {
				t.Errorf("%s: %v: expected config %s to have value \"%s\", not \"%s\"", caller, result, expEntry.Name, expEntry.Value, entry.Value)
				continue
			}

			matchCnt++
		}

		if matchCnt != len(expResult.Config) {
			t.Errorf("%s: %v: only %d/%d expected configs matched", caller, result, matchCnt, len(expResult.Config))
		}
	}

	if t.Failed() {
		t.Fatalf("%s: ConfigResourceResult validation failed: see previous errors", caller)
	}
}

type IntegrationTestSuite struct {
	suite.Suite
	compose *compose.LocalDockerCompose
}

func (its *IntegrationTestSuite) TearDownSuite() {
	if testconf.DockerNeeded && its.compose != nil {
		its.compose.Down()
	}
}

// TestConsumerSeekPartitions tests seeking of partitions using SeekPartitions().
func (its *IntegrationTestSuite) TestConsumerSeekPartitions() {
	t := its.T()

	numMessages := 10 // should be more than or equal to 2.

	// Produce `numMessages` messages to Topic.
	conf := ConfigMap{"bootstrap.servers": testconf.Brokers}
	conf.updateFromTestconf()

	producer, err := NewProducer(&conf)
	if err != nil {
		t.Fatalf("Failed to create producer: %s", err)
	}

	for idx := 0; idx < numMessages; idx++ {
		if err = producer.Produce(&Message{
			TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: 0},
		}, nil); err != nil {
			t.Fatalf("Failed to produce message: %s", err)
		}
	}

	producer.Flush(10 * 1000)
	producer.Close()

	// Assign partition, seek to `numMessages`/2, and check by reading the message.
	conf = ConfigMap{
		"bootstrap.servers": testconf.Brokers,
		"group.id":          testconf.GroupID,
		"auto.offset.reset": "end",
	}
	conf.updateFromTestconf()

	consumer, err := testNewConsumer(t, &conf)
	if err != nil {
		t.Fatalf("Failed to create consumer: %s", err)
	}

	tps := []TopicPartition{
		{Topic: &testconf.TopicName, Partition: 0},
	}
	err = consumer.Assign(tps)
	if err != nil {
		t.Fatalf("Failed to assign partition: %s", err)
	}

	var leaderEpoch int32 = 0
	tps[0].Offset = Offset(numMessages / 2)
	tps[0].LeaderEpoch = &leaderEpoch
	seekedPartitions, err := consumer.SeekPartitions(tps)
	if err != nil {
		t.Errorf("SeekPartitions failed: %s", err)
	}
	if len(seekedPartitions) != len(tps) {
		t.Errorf(
			"SeekPartitions should return result for %d partitions, %d returned",
			len(tps), len(seekedPartitions))
	}
	for _, seekedPartition := range seekedPartitions {
		if seekedPartition.Error != nil {
			t.Errorf("Seek error for partition %v", seekedPartition)
		}
	}

	msg, err := consumer.ReadMessage(10 * time.Second)
	if err != nil {
		t.Fatalf("ReadMessage failed: %s", err)
	}
	if msg.TopicPartition.Offset != Offset(numMessages/2) {
		t.Errorf("Expected offset of read message is %d, got %d",
			numMessages/2, msg.TopicPartition.Offset)
	}

	if msg.TopicPartition.LeaderEpoch == nil {
		t.Errorf("Expected leader epoch got nil")
		return
	}

	if *msg.TopicPartition.LeaderEpoch != 0 {
		t.Errorf("Expected leader epoch of read message is %d, got %d",
			0, *msg.TopicPartition.LeaderEpoch)
	}
}

// TestAdminClient_DeleteConsumerGroups verifies the working of the
// DeleteConsumerGroups API in the admin client.
// It does so by listing consumer groups before/after deletion.
func (its *IntegrationTestSuite) TestAdminClient_DeleteConsumerGroups() {
	t := its.T()
	rand.Seed(time.Now().Unix())

	// Generating new groupID to ensure a fresh group is created.
	groupID := fmt.Sprintf("%s-%d", testconf.GroupID, rand.Int())

	ac := createAdminClient(t)
	testTopicName := createTestTopic(t, testconf.TopicName+".TestAdminClient_DeleteConsumerGroups", 3, 1)
	defer ac.Close()

	// Check that our group is not present initially.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	listGroupResult, err := ac.ListConsumerGroups(ctx, SetAdminRequestTimeout(30*time.Second))
	if err != nil {
		t.Errorf("Error listing consumer groups %s\n", err)
		return
	}

	if findConsumerGroupListing(listGroupResult.Valid, groupID) != nil {
		t.Errorf("Consumer group present before consumer created: %s\n", groupID)
		return
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	// Create consumer
	config := &ConfigMap{
		"bootstrap.servers":        testconf.Brokers,
		"group.id":                 groupID,
		"auto.offset.reset":        "earliest",
		"enable.auto.offset.store": false,
	}
	config.updateFromTestconf()
	consumer, err := testNewConsumer(t, config)
	if err != nil {
		t.Errorf("Failed to create consumer: %s\n", err)
		return
	}
	consumerClosed := false
	defer func() {
		if !consumerClosed {
			consumer.Close()
		}
	}()

	if err := consumer.Subscribe(testTopicName, nil); err != nil {
		t.Errorf("Failed to subscribe to %s: %s\n", testTopicName, err)
		return
	}

	// This ReadMessage gives some time for the rebalance to happen.
	_, err = consumer.ReadMessage(5 * time.Second)
	if err != nil && err.(Error).Code() != ErrTimedOut {
		t.Errorf("Failed while reading message: %s\n", err)
		return
	}

	// Check that the group exists.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	listGroupResult, err = ac.ListConsumerGroups(ctx, SetAdminRequestTimeout(30*time.Second))
	if err != nil {
		t.Errorf("Error listing consumer groups %s\n", err)
		return
	}

	if findConsumerGroupListing(listGroupResult.Valid, groupID) == nil {
		t.Errorf("Consumer group %s should be present\n", groupID)
		return
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	// Try deleting the group while consumer is active. It should fail.
	result, err := ac.DeleteConsumerGroups(ctx, []string{groupID})
	if err != nil {
		t.Errorf("DeleteConsumerGroups() failed: %s", err)
		return
	}
	resultGroups := result.ConsumerGroupResults

	if len(resultGroups) != 1 || resultGroups[0].Group != groupID {
		t.Errorf("Wrong group affected/no group affected")
		return
	}

	if resultGroups[0].Error.code != ErrNonEmptyGroup {
		t.Errorf("Encountered the wrong error after calling DeleteConsumerGroups %s", resultGroups[0].Error)
		return
	}

	// Close the consumer.
	if err = consumer.Close(); err != nil {
		t.Errorf("Could not close consumer %s", err)
		return
	}
	consumerClosed = true

	// Delete the consumer group now.
	result, err = ac.DeleteConsumerGroups(ctx, []string{groupID})
	if err != nil {
		t.Errorf("DeleteConsumerGroups() failed: %s", err)
		return
	}
	resultGroups = result.ConsumerGroupResults

	if len(resultGroups) != 1 || resultGroups[0].Group != groupID {
		t.Errorf("Wrong group affected/no group affected")
		return
	}

	if resultGroups[0].Error.code != ErrNoError {
		t.Errorf("Encountered an error after calling DeleteConsumerGroups %s", resultGroups[0].Error)
		return
	}

	// Check for the absence of the consumer group after deletion.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	listGroupResult, err = ac.ListConsumerGroups(ctx, SetAdminRequestTimeout(30*time.Second))
	if err != nil {
		t.Errorf("Error listing consumer groups %s\n", err)
		return
	}

	if findConsumerGroupListing(listGroupResult.Valid, groupID) != nil {
		t.Errorf("Consumer group %s should not be present\n", groupID)
		return
	}
	return
}

// TestAdminClient_ListConsumerGroups validates the working of the
// list consumer groups API of the admin client.
//
// We test the following situations:
//  1. when creating a consumer of type T in group G and listing with that type,
//     all returned groups should be of type T and G must be included.
//  2. when creating a consumer of type T and listing with the opposite type,
//     all returned groups should be of type opposite of T and G must not be included.
func (its *IntegrationTestSuite) TestAdminClient_ListConsumerGroups() {
	t := its.T()

	usedType := ConsumerGroupTypeClassic
	oppositeType := ConsumerGroupTypeConsumer
	if !testConsumerGroupProtocolClassic() {
		usedType = ConsumerGroupTypeConsumer
		oppositeType = ConsumerGroupTypeClassic
	}

	groupID := fmt.Sprintf("%s-%d", testconf.GroupID, rand.Int())
	topic := fmt.Sprintf("%s-%d", testconf.TopicName, rand.Int())

	clientID := "test.client"

	ac := createAdminClient(t)
	defer ac.Close()

	// Create a topic
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := ac.CreateTopics(ctx, []TopicSpecification{
		{
			Topic:         topic,
			NumPartitions: 1,
		},
	})
	if err != nil {
		t.Errorf("Topic creation failed with error %v", err)
		return
	}

	// Delete the topic after the test is done.
	defer func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = ac.DeleteTopics(ctx, []string{topic})
		if err != nil {
			t.Errorf("Topic deletion failed with error %v", err)
		}
	}()

	config := &ConfigMap{
		"bootstrap.servers": testconf.Brokers,
		"group.id":          groupID,
		"client.id":         clientID,
	}
	config.updateFromTestconf()
	consumer, err := testNewConsumer(t, config)
	if err != nil {
		t.Errorf("Failed to create consumer: %s\n", err)
		return
	}
	consumerClosed := false
	defer func() {
		if !consumerClosed {
			consumer.Close()
		}
	}()
	consumer.Subscribe(topic, nil)

	// Call Poll to trigger a rebalance and give it enough time to finish.
	consumer.Poll(10 * 1000)

	// Our Consumer should be listed when searching groups of the same type.
	listGroupResult, err := ac.ListConsumerGroups(ctx, SetAdminRequestTimeout(30*time.Second),
		SetAdminMatchConsumerGroupTypes([]ConsumerGroupType{usedType}))
	if err != nil || len(listGroupResult.Errors) > 0 {
		t.Errorf("Error listing consumer groups %s %v\n", err, listGroupResult.Errors)
		return
	}
	groups := listGroupResult.Valid
	if findConsumerGroupListing(groups, groupID) == nil {
		t.Errorf("Consumer groups %s should be present\n", groupID)
		return
	}
	for _, groupInfo := range groups {
		if groupInfo.Type != usedType {
			fmt.Printf("GroupName : %s, Type : %d", groupInfo.GroupID, groupInfo.Type)
			t.Errorf("Consumer group type should be %s\n", usedType)
			return
		}
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	// Check opposite group type and current group shouldn't be found.
	listGroupResult, err = ac.ListConsumerGroups(ctx, SetAdminRequestTimeout(30*time.Second),
		SetAdminMatchConsumerGroupTypes([]ConsumerGroupType{oppositeType}))
	if err != nil || len(listGroupResult.Errors) > 0 {
		t.Errorf("Error listing consumer groups %s %v\n", err, listGroupResult.Errors)
		return
	}
	groups = listGroupResult.Valid
	for _, groupInfo := range groups {
		if groupInfo.Type != oppositeType {
			fmt.Printf("GroupName : %s, Type : %d", groupInfo.GroupID, groupInfo.Type)
			t.Errorf("Consumer group type should be %s\n", oppositeType)
			return
		}
	}
	if findConsumerGroupListing(groups, groupID) != nil {
		t.Errorf("Consumer group %s should not be present as the Type filter "+
			"was set to the opposite one: %s\n", groupID, oppositeType)
		return
	}
}

// TestAdminClient_ListAndDescribeConsumerGroups validates the working of the
// list consumer groups and describe consumer group APIs of the admin client.
//
// We test the following situations:
//
// 1. One consumer group with one client.
// 2. One consumer group with two clients.
// 3. Empty consumer group.
func (its *IntegrationTestSuite) TestAdminClient_ListAndDescribeConsumerGroups() {
	t := its.T()
	if !testConsumerGroupProtocolClassic() {
		t.Skipf("KIP 848 Admin operations changes still aren't " +
			"available")
	}

	// Generating a new topic/groupID to ensure a fresh group/topic is created.
	rand.Seed(time.Now().Unix())
	groupID := fmt.Sprintf("%s-%d", testconf.GroupID, rand.Int())
	topic := fmt.Sprintf("%s-%d", testconf.TopicName, rand.Int())
	nonExistentGroupID := fmt.Sprintf("%s-nonexistent-%d", testconf.GroupID, rand.Int())

	clientID1 := "test.client.1"
	clientID2 := "test.client.2"

	ac := createAdminClient(t)
	defer ac.Close()

	// Create a topic - we need to create here because we need 2 partitions.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := ac.CreateTopics(ctx, []TopicSpecification{
		{
			Topic:         topic,
			NumPartitions: 2,
		},
	})
	if err != nil {
		t.Errorf("Topic creation failed with error %v", err)
		return
	}

	// Delete the topic after the test is done.
	defer func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = ac.DeleteTopics(ctx, []string{topic})
		if err != nil {
			t.Errorf("Topic deletion failed with error %v", err)
		}
	}()

	// Check the non-existence of consumer groups initially.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	listGroupResult, err := ac.ListConsumerGroups(ctx, SetAdminRequestTimeout(30*time.Second))
	if err != nil || len(listGroupResult.Errors) > 0 {
		t.Errorf("Error listing consumer groups %s %v\n", err, listGroupResult.Errors)
		return
	}

	groups := listGroupResult.Valid
	if findConsumerGroupListing(groups, groupID) != nil || findConsumerGroupListing(groups, nonExistentGroupID) != nil {
		t.Errorf("Consumer groups %s and %s should not be present\n", groupID, nonExistentGroupID)
		return
	}

	// 1. One consumer group with one client.
	// Create the first consumer.
	config := &ConfigMap{
		"bootstrap.servers":             testconf.Brokers,
		"group.id":                      groupID,
		"auto.offset.reset":             "earliest",
		"enable.auto.offset.store":      false,
		"client.id":                     clientID1,
		"partition.assignment.strategy": "range",
	}
	config.updateFromTestconf()
	consumer1, err := testNewConsumer(t, config)
	if err != nil {
		t.Errorf("Failed to create consumer: %s\n", err)
		return
	}
	consumer1Closed := false
	defer func() {
		if !consumer1Closed {
			consumer1.Close()
		}
	}()
	consumer1.Subscribe(topic, nil)

	// Call Poll to trigger a rebalance and give it enough time to finish.
	consumer1.Poll(10 * 1000)

	// Check the existence of the group.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	listGroupResult, err = ac.ListConsumerGroups(ctx, SetAdminRequestTimeout(30*time.Second))
	if err != nil || len(listGroupResult.Errors) > 0 {
		t.Errorf("Error listing consumer groups %s %v\n", err, listGroupResult.Errors)
		return
	}
	groups = listGroupResult.Valid

	if findConsumerGroupListing(groups, groupID) == nil || findConsumerGroupListing(groups, nonExistentGroupID) != nil {
		t.Errorf("Consumer groups %s should be present and %s should not be\n", groupID, nonExistentGroupID)
		return
	}

	// Test the description of the consumer group.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	groupDescResult, err := ac.DescribeConsumerGroups(
		ctx, []string{groupID}, SetAdminRequestTimeout(30*time.Second))
	if err != nil {
		t.Errorf("Error describing consumer groups %s\n", err)
		return
	}

	groupDescs := groupDescResult.ConsumerGroupDescriptions
	if len(groupDescs) != 1 {
		t.Errorf("Describing one group should give exactly one result %s\n", err)
		return
	}

	groupDesc := &groupDescs[0]

	clientIDToPartitions := make(map[string][]TopicPartition)
	clientIDToPartitions[clientID1] = []TopicPartition{
		{Topic: &topic, Partition: 0, Offset: OffsetInvalid},
		{Topic: &topic, Partition: 1, Offset: OffsetInvalid},
	}
	if !checkGroupDesc(groupDesc, ConsumerGroupStateStable, groupID, "range", clientIDToPartitions) {
		t.Errorf("Expected description for consumer group  %s is not same as actual: %v", groupID, groupDesc)
		return
	}

	// 2. One consumer group with two clients.
	// Add another consumer to the same group.
	config = &ConfigMap{
		"bootstrap.servers":             testconf.Brokers,
		"group.id":                      groupID,
		"auto.offset.reset":             "earliest",
		"enable.auto.offset.store":      false,
		"client.id":                     clientID2,
		"partition.assignment.strategy": "range",
	}
	config.updateFromTestconf()
	consumer2, err := testNewConsumer(t, config)
	if err != nil {
		t.Errorf("Failed to create consumer: %s\n", err)
		return
	}
	consumer2Closed := false
	defer func() {
		if !consumer2Closed {
			consumer2.Close()
		}
	}()
	consumer2.Subscribe(topic, nil)

	// Call Poll to start triggering the rebalance. Give it enough time to run
	// that it becomes stable.
	// We need to make sure that the consumer group stabilizes since we will
	// check for the state later on.
	consumer2.Poll(5 * 1000)
	consumer1.Poll(5 * 1000)
	isGroupStable := false
	for !isGroupStable {
		ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		groupDescResult, err = ac.DescribeConsumerGroups(ctx, []string{groupID}, SetAdminRequestTimeout(30*time.Second))
		if err != nil {
			t.Errorf("Error describing consumer groups %s\n", err)
			return
		}
		groupDescs = groupDescResult.ConsumerGroupDescriptions
		groupDesc = findConsumerGroupDescription(groupDescs, groupID)
		if groupDesc == nil {
			t.Errorf("Consumer group %s should be present\n", groupID)
			return
		}
		isGroupStable = groupDesc.State == ConsumerGroupStateStable
		time.Sleep(time.Second)
	}

	clientIDToPartitions[clientID1] = []TopicPartition{
		{Topic: &topic, Partition: 0, Offset: OffsetInvalid},
	}
	clientIDToPartitions[clientID2] = []TopicPartition{
		{Topic: &topic, Partition: 1, Offset: OffsetInvalid},
	}
	if !checkGroupDesc(groupDesc, ConsumerGroupStateStable, groupID, "range", clientIDToPartitions) {
		t.Errorf("Expected description for consumer group  %s is not same as actual %v\n", groupID, groupDesc)
		return
	}

	// 3. Empty consumer group.
	// Close the existing consumers.
	if consumer1.Close() != nil {
		t.Errorf("Error closing the first consumer\n")
		return
	}
	consumer1Closed = true

	if consumer2.Close() != nil {
		t.Errorf("Error closing the second consumer\n")
		return
	}
	consumer2Closed = true

	// Try describing an empty group.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	groupDescResult, err = ac.DescribeConsumerGroups(ctx, []string{groupID}, SetAdminRequestTimeout(30*time.Second))
	groupDescs = groupDescResult.ConsumerGroupDescriptions

	if err != nil {
		t.Errorf("Error describing consumer groups %s\n", err)
		return
	}

	groupDesc = findConsumerGroupDescription(groupDescs, groupID)
	if groupDesc == nil {
		t.Errorf("Consumer group %s should be present\n", groupID)
		return
	}

	clientIDToPartitions = make(map[string][]TopicPartition)
	if !checkGroupDesc(groupDesc, ConsumerGroupStateEmpty, groupID, "", clientIDToPartitions) {
		t.Errorf("Expected description for consumer group  %s is not same as actual %v\n", groupID, groupDesc)
	}

	// Try listing the Empty consumer group, and make sure that the States option
	// works while listing.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	listGroupResult, err = ac.ListConsumerGroups(
		ctx, SetAdminRequestTimeout(30*time.Second),
		SetAdminMatchConsumerGroupStates([]ConsumerGroupState{ConsumerGroupStateEmpty}))
	if err != nil || len(listGroupResult.Errors) > 0 {
		t.Errorf("Error listing consumer groups %s %v\n", err, listGroupResult.Errors)
		return
	}
	groups = listGroupResult.Valid

	groupInfo := findConsumerGroupListing(listGroupResult.Valid, groupID)
	if groupInfo == nil {
		t.Errorf("Consumer group %s should be present\n", groupID)
		return
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	listGroupResult, err = ac.ListConsumerGroups(
		ctx, SetAdminRequestTimeout(30*time.Second),
		SetAdminMatchConsumerGroupStates([]ConsumerGroupState{ConsumerGroupStateStable}))
	if err != nil || len(listGroupResult.Errors) > 0 {
		t.Errorf("Error listing consumer groups %s %v\n", err, listGroupResult.Errors)
		return
	}
	groups = listGroupResult.Valid

	groupInfo = findConsumerGroupListing(groups, groupID)
	if groupInfo != nil {
		t.Errorf("Consumer group %s should not be present\n", groupID)
		return
	}
}

// TestAdminClient_DescribeConsumerGroupsAuthorizedOperations validates the
// working of the DescribeConsumerGroups API of the admin client for fetching
// authorized operations (KIP-430).
//
// We test the following situations:
//
// 1. Default ACLs on group.
// 2. Modified ACLs on group.
func (its *IntegrationTestSuite) TestAdminClient_DescribeConsumerGroupsAuthorizedOperations() {
	t := its.T()
	assert := its.Assert()

	// Generating a new topic/groupID to ensure a fresh group/topic is created.
	rand.Seed(time.Now().Unix())
	groupID := fmt.Sprintf("%s-%d", testconf.GroupID, rand.Int())
	topic := fmt.Sprintf("%s-%d", testconf.TopicName, rand.Int())

	clientID := "test.client.1"

	ac := createAdminClientWithSasl(t)
	defer ac.Close()

	// Create a topic.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := ac.CreateTopics(ctx, []TopicSpecification{
		{
			Topic:         topic,
			NumPartitions: 2,
		},
	})
	assert.Nil(err, "CreateTopics should succeed")

	// Delete the topic after the test is done.
	defer func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = ac.DeleteTopics(ctx, []string{topic})
		assert.Nil(err, "DeleteTopics should succeed")
	}()

	// Create a consumer so that a consumer group might be created
	config := &ConfigMap{
		"bootstrap.servers": testconf.BrokersSasl,
		"group.id":          groupID,
		"client.id":         clientID,
		"sasl.username":     testconf.SaslUsername,
		"sasl.password":     testconf.SaslPassword,
		"sasl.mechanism":    testconf.SaslMechanism,
		"security.protocol": "SASL_PLAINTEXT",
	}
	config.updateFromTestconf()
	consumer, err := testNewConsumer(t, config)
	assert.Nil(err, "NewConsumer should succeed")

	// Close the consumer after the test is done
	defer consumer.Close()

	consumer.Subscribe(topic, nil)

	// Call Poll to trigger a rebalance and give it enough time to finish.
	consumer.Poll(10 * 1000)

	// 1. Default ACLs on group.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	groupDescResult, err := ac.DescribeConsumerGroups(
		ctx, []string{groupID}, SetAdminRequestTimeout(30*time.Second),
		SetAdminOptionIncludeAuthorizedOperations(true))
	assert.Nil(err, "DescribeConsumerGroups should succeed")

	groupDescs := groupDescResult.ConsumerGroupDescriptions
	assert.Len(groupDescs, 1, "Describing one group should give exactly one result")

	groupDesc := &groupDescs[0]
	assert.Equal(ErrNoError, groupDesc.Error.Code(),
		"Group description should succeed, failed with %v", groupDesc.Error)
	assert.NotEmpty(groupDesc.AuthorizedOperations,
		"Authorized operations should not be empty")

	expectedOperations := []ACLOperation{
		ACLOperationRead, ACLOperationDelete, ACLOperationDescribe}
	// The default allowed operations depend on the Authorizer, which is
	// different for KRaft and ZK. Unfortunately, there is no surefire way to
	// tell if the cluster uses ZK or KRaft unless we have created it, so we just
	// match the common subset, and then do conditional matching.
	// FIXME: add test parameter to identify KRaft being used.
	assert.Subset(groupDesc.AuthorizedOperations, expectedOperations)

	// Check if the authorized operations contain ACLOperationDescribeConfigs, if
	// so it must contain ACLOperationAlterConfigs, as those are the two extra
	// ones in KRaft.
	if slices.Contains(groupDesc.AuthorizedOperations, ACLOperationDescribeConfigs) {
		assert.Contains(groupDesc.AuthorizedOperations, ACLOperationAlterConfigs)
	}

	// Change the ACLs on the group
	newACLs := ACLBindings{
		{
			Type:                ResourceGroup,
			Name:                groupID,
			ResourcePatternType: ResourcePatternTypeLiteral,
			Principal:           "User:*",
			Host:                "*",
			Operation:           ACLOperationRead,
			PermissionType:      ACLPermissionTypeAllow,
		},
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	resultCreateACLs, err := ac.CreateACLs(ctx, newACLs,
		SetAdminRequestTimeout(time.Second))
	assert.Nil(err, "CreateACLs should not throw an error")
	assert.Len(resultCreateACLs, 1,
		"CreateACLs result should contain on result")
	assert.Equal(
		resultCreateACLs[0].Error.Code(), ErrNoError,
		"CreateACLs result should not have an error")

	// Delete group ACLs to keep the test cluster clean.
	defer func() {
		aclBindingFilters := ACLBindingFilters{
			{
				Type:                ResourceGroup,
				Name:                groupID,
				ResourcePatternType: ResourcePatternTypeLiteral,
				Principal:           "User:*",
				Host:                "*",
				Operation:           ACLOperationRead,
				PermissionType:      ACLPermissionTypeAllow,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		_, err = ac.DeleteACLs(ctx, aclBindingFilters,
			SetAdminRequestTimeout(time.Second*30))
		assert.Nil(err, "DeleteACLs should not throw an error")
	}()

	// 2. Modified ACLs on group.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	groupDescResult, err = ac.DescribeConsumerGroups(
		ctx, []string{groupID}, SetAdminRequestTimeout(30*time.Second),
		SetAdminOptionIncludeAuthorizedOperations(true))
	assert.Nil(err, "DescribeConsumerGroups should succeed")

	groupDescs = groupDescResult.ConsumerGroupDescriptions
	assert.Len(groupDescs, 1,
		"Describing one group should give exactly one result")

	groupDesc = &groupDescs[0]
	assert.NotEmpty(groupDesc.AuthorizedOperations,
		"Authorized operations should not be empty")
	// Read permissions implicitly allows Describe.
	assert.ElementsMatch(groupDesc.AuthorizedOperations,
		[]ACLOperation{ACLOperationRead, ACLOperationDescribe})
}

// TestAdminClient_DescribeCluster validates the working of the
// DescribeCluster API of the admin client.
//
// We test the following situations:
//
// 1. DescribeCluster without ACLs.
// 2. DescribeCluster with default ACLs.
// 3. DescribeCluster with modified ACLs.
func (its *IntegrationTestSuite) TestAdminClient_DescribeCluster() {
	t := its.T()
	assert := its.Assert()
	ac := createAdminClient(t)
	defer ac.Close()

	// 1. DescribeCluster without ACLs.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	descres, err := ac.DescribeCluster(
		ctx, SetAdminRequestTimeout(time.Second*30))

	// There are fields which cannot be checked, like controller, or the value
	// of the cluster ID. We try checking for the existence in cases we can.
	assert.Nil(err, "DescribeCluster should not throw an error")
	assert.NotEmpty(descres.Nodes, "Cluster nodes should not be empty")
	assert.NotEmpty(descres.ClusterID, "Cluster id should be set")
	assert.NotEmpty(descres.Nodes[0].Host,
		"First node's host should be non-empty")
	assert.Empty(descres.AuthorizedOperations,
		"Authorized operations should be empty, not requested")

	// Tests for Authorized Operations need a broker with SASL authentication.
	// This may be a different broker than the usual broker, so we create a
	// new AdminClient.
	ac = createAdminClientWithSasl(t)
	defer ac.Close()

	// 2. DescribeCluster with default ACLs.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	descres, err = ac.DescribeCluster(
		ctx, SetAdminRequestTimeout(time.Second*30),
		SetAdminOptionIncludeAuthorizedOperations(true))

	assert.Nil(err, "DescribeCluster should not throw an error")
	assert.NotEmpty(descres.Nodes, "Cluster nodes should not be empty")
	assert.NotEmpty(descres.ClusterID, "Cluster id should be set")
	assert.NotEmpty(descres.Nodes[0].Host,
		"First node's host should be non-empty")
	assert.NotEmpty(descres.AuthorizedOperations,
		"Authorized operations should not be empty")
	assert.ElementsMatch(descres.AuthorizedOperations,
		[]ACLOperation{
			ACLOperationCreate, ACLOperationAlter, ACLOperationDescribe,
			ACLOperationClusterAction, ACLOperationDescribeConfigs,
			ACLOperationAlterConfigs, ACLOperationIdempotentWrite})

	// Create some ACL bindings on the cluster.
	newACLs := ACLBindings{
		{
			Type:                ResourceBroker,
			Name:                "kafka-cluster",
			ResourcePatternType: ResourcePatternTypeLiteral,
			Principal:           "User:*",
			Host:                "*",
			Operation:           ACLOperationAlter,
			PermissionType:      ACLPermissionTypeAllow,
		},
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	resultCreateACLs, err := ac.CreateACLs(ctx, newACLs,
		SetAdminRequestTimeout(time.Second*30))
	assert.Nil(err, "CreateACLs should not throw an error")
	assert.Len(resultCreateACLs, 1,
		"CreateACLs result should contain on result")
	assert.Equal(
		resultCreateACLs[0].Error.Code(), ErrNoError,
		"CreateACLs result should not have an error")

	// Clean up cluster ACLs for subsequent tests.
	defer func() {
		aclBindingFilters := ACLBindingFilters{
			{
				Type:                ResourceBroker,
				Name:                "kafka-cluster",
				ResourcePatternType: ResourcePatternTypeMatch,
				Principal:           "User:*",
				Host:                "*",
				Operation:           ACLOperationAlter,
				PermissionType:      ACLPermissionTypeAllow,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		_, err = ac.DeleteACLs(ctx, aclBindingFilters,
			SetAdminRequestTimeout(time.Second*30))
		assert.Nil(err, "DeleteACLs should not throw an error")
	}()

	// 3. DescribeCluster with modified ACLs.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	descres, err = ac.DescribeCluster(
		ctx, SetAdminRequestTimeout(time.Second*30),
		SetAdminOptionIncludeAuthorizedOperations(true))

	assert.Nil(err, "DescribeCluster should not throw an error")
	assert.NotEmpty(descres.Nodes, "Cluster nodes should not be empty")
	assert.NotEmpty(descres.ClusterID, "Cluster id should be set")
	assert.NotEmpty(descres.Nodes[0].Host,
		"First node's host should be non-empty")
	assert.NotEmpty(descres.AuthorizedOperations,
		"Authorized operations should not be empty")
	// Alter permissions implicitly allow Describe.
	assert.ElementsMatch(descres.AuthorizedOperations,
		[]ACLOperation{ACLOperationDescribe, ACLOperationAlter})
}

// TestAdminClient_DescribeTopics validates the working of the
// DescribeTopics API of the admin client.
//
// We test the following situations:
//
// 1. DescribeTopics without ACLs.
// 2. DescribeTopics with default ACLs.
// 3. DescribeTopics with modified ACLs.
func (its *IntegrationTestSuite) TestAdminClient_DescribeTopics() {
	t := its.T()
	assert := its.Assert()
	rand.Seed(time.Now().Unix())

	ac := createAdminClient(t)
	defer ac.Close()

	// Create a topic
	topic := fmt.Sprintf("%s-%d", testconf.TopicName, rand.Int())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := ac.CreateTopics(ctx, []TopicSpecification{
		{
			Topic:         topic,
			NumPartitions: 2,
		},
	})
	assert.Nil(err, "CreateTopics should not fail")

	// Wait for propagation
	time.Sleep(1 * time.Second)

	// Delete the topic after the test is done.
	defer func(ac *AdminClient) {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = ac.DeleteTopics(ctx, []string{topic})
		assert.Nil(err, "DeleteTopics should not fail")
	}(ac)

	// 1. DescribeTopics without ACLs.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	topicDescResult, err := ac.DescribeTopics(
		ctx, NewTopicCollectionOfTopicNames([]string{topic, "nonexistent"}),
		SetAdminRequestTimeout(30*time.Second))
	assert.Nil(err, "DescribeTopics should not fail")

	topicDescs := topicDescResult.TopicDescriptions
	assert.Len(topicDescs, 2,
		"Describing two topics should give exactly two results")
	assert.Equal(topicDescs[0].Name, topic,
		"First result topic should match request topic")
	assert.Equal(topicDescs[1].Name, "nonexistent",
		"Second result topic should match request topic")
	assert.Equal(topicDescs[1].Error.Code(), ErrUnknownTopicOrPart,
		"Expected correct error for nonexistent topic")

	topicDesc := topicDescs[0]
	assert.NotZero(topicDesc.TopicID.GetLeastSignificantBits())
	assert.NotZero(topicDesc.TopicID.GetMostSignificantBits())
	assert.NotEmpty(topicDesc.TopicID.String())
	assert.Equal(topicDesc.Error.Code(), ErrNoError,
		"Topic description should not have an error")
	assert.False(topicDesc.IsInternal, "Topic should not be internal")
	assert.Empty(topicDesc.AuthorizedOperations,
		"Topic should not have authorized operations")

	assert.Len(topicDesc.Partitions, 2, "Topic should have two partitions")
	assert.GreaterOrEqual(len(topicDesc.Partitions[0].Replicas), 1,
		"At least one replica should exist for partition")

	// Tests for Authorized Operations need a broker with SASL authentication.
	// This may be a different broker than the usual broker, so we create a
	// new AdminClient.
	ac = createAdminClientWithSasl(t)
	defer ac.Close()

	// Create a topic - the broker may be different for SASL, so we need to
	// ensure that a topic is created.
	topic = fmt.Sprintf("%s-%d", testconf.TopicName, rand.Int())
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = ac.CreateTopics(ctx, []TopicSpecification{
		{
			Topic:         topic,
			NumPartitions: 2,
		},
	})
	assert.Nil(err, "CreateTopics should not fail")

	// Wait for propagation
	time.Sleep(1 * time.Second)

	// Delete the second topic after the test is done.
	defer func(ac *AdminClient) {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = ac.DeleteTopics(ctx, []string{topic})
		assert.Nil(err, "DeleteTopics should not fail")
	}(ac)

	// 2. DescribeTopics with default ACLs.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	topicDescResult, err = ac.DescribeTopics(
		ctx, NewTopicCollectionOfTopicNames([]string{topic}),
		SetAdminRequestTimeout(30*time.Second),
		SetAdminOptionIncludeAuthorizedOperations(true))

	assert.Nil(err, "DescribeTopics should not fail")

	topicDescs = topicDescResult.TopicDescriptions
	assert.Len(topicDescs, 1,
		"Describing one topic should give exactly one result")
	assert.Equal(topicDescs[0].Name, topic,
		"First result topic should match request topic")

	topicDesc = topicDescs[0]
	assert.Equal(topicDesc.Error.Code(), ErrNoError,
		"Topic description should not have an error")
	assert.NotEmpty(topicDesc.AuthorizedOperations,
		"Topic should have authorized operations")
	assert.ElementsMatch(topicDesc.AuthorizedOperations, []ACLOperation{
		ACLOperationRead, ACLOperationWrite, ACLOperationCreate,
		ACLOperationDelete, ACLOperationAlter, ACLOperationDescribe,
		ACLOperationDescribeConfigs, ACLOperationAlterConfigs})

	// Create some ACL bindings on the topic.
	newACLs := ACLBindings{
		{
			Type:                ResourceTopic,
			Name:                topic,
			ResourcePatternType: ResourcePatternTypeLiteral,
			Principal:           "User:*",
			Host:                "*",
			Operation:           ACLOperationRead,
			PermissionType:      ACLPermissionTypeAllow,
		},
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	resultCreateACLs, err := ac.CreateACLs(ctx, newACLs,
		SetAdminRequestTimeout(time.Second))
	assert.Nil(err, "CreateACLs should not throw an error")
	assert.Len(resultCreateACLs, 1,
		"CreateACLs result should contain on result")
	assert.Equal(
		resultCreateACLs[0].Error.Code(), ErrNoError,
		"CreateACLs result should not have an error")

	// Delete topic ACLs to keep the test cluster clean.
	defer func() {
		aclBindingFilters := ACLBindingFilters{
			{
				Type:                ResourceTopic,
				Name:                topic,
				ResourcePatternType: ResourcePatternTypeLiteral,
				Principal:           "User:*",
				Host:                "*",
				Operation:           ACLOperationRead,
				PermissionType:      ACLPermissionTypeAllow,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		_, err = ac.DeleteACLs(ctx, aclBindingFilters,
			SetAdminRequestTimeout(time.Second*30))
		assert.Nil(err, "DeleteACLs should not throw an error")
	}()

	// 3. DescribeTopics with modified ACLs.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	topicDescResult, err = ac.DescribeTopics(
		ctx, NewTopicCollectionOfTopicNames([]string{topic}),
		SetAdminRequestTimeout(time.Second*30),
		SetAdminOptionIncludeAuthorizedOperations(true))

	assert.Nil(err, "DescribeTopics should not fail")

	topicDescs = topicDescResult.TopicDescriptions
	assert.Len(topicDescs, 1,
		"Describing one topic should give exactly one result")
	assert.Equal(topicDescs[0].Name, topic,
		"First result topic should match request topic")

	topicDesc = topicDescs[0]
	assert.Equal(topicDesc.Error.Code(), ErrNoError,
		"Topic description should not have an error")
	assert.NotEmpty(topicDesc.AuthorizedOperations,
		"Topic should have authorized operations")
	// Read permissions implicitly allows Describe.
	assert.ElementsMatch(topicDesc.AuthorizedOperations,
		[]ACLOperation{ACLOperationRead, ACLOperationDescribe})
}

func (its *IntegrationTestSuite) TestAdminTopics() {
	t := its.T()
	rand.Seed(time.Now().Unix())

	a := createAdminClient(t)
	defer a.Close()

	brokerList, err := getBrokerList(a)
	if err != nil {
		t.Fatalf("Failed to retrieve broker list: %v", err)
	}

	// Few and Many replica sets use in these tests
	var fewReplicas []int32
	if len(brokerList) < 2 {
		fewReplicas = brokerList
	} else {
		fewReplicas = brokerList[0:2]
	}

	var manyReplicas []int32
	if len(brokerList) < 5 {
		manyReplicas = brokerList
	} else {
		manyReplicas = brokerList[0:5]
	}

	const topicCnt = 7
	newTopics := make([]TopicSpecification, topicCnt)

	expError := map[string]Error{}

	for i := 0; i < topicCnt; i++ {
		topic := fmt.Sprintf("%s-create-%d-%d", testconf.TopicName, i, rand.Intn(100000))
		newTopics[i] = TopicSpecification{
			Topic:         topic,
			NumPartitions: 1 + i*2,
		}

		if (i % 1) == 0 {
			newTopics[i].ReplicationFactor = len(fewReplicas)
		} else {
			newTopics[i].ReplicationFactor = len(manyReplicas)
		}

		expError[newTopics[i].Topic] = Error{} // No error

		var useReplicas []int32
		if i == 2 {
			useReplicas = fewReplicas
		} else if i == 3 {
			useReplicas = manyReplicas
		} else if i == topicCnt-1 {
			newTopics[i].ReplicationFactor = len(brokerList) + 10
			expError[newTopics[i].Topic] = Error{code: ErrInvalidReplicationFactor}
		}

		if len(useReplicas) > 0 {
			newTopics[i].ReplicaAssignment = make([][]int32, newTopics[i].NumPartitions)
			newTopics[i].ReplicationFactor = 0
			for p := 0; p < newTopics[i].NumPartitions; p++ {
				newTopics[i].ReplicaAssignment[p] = useReplicas
			}
		}
	}

	maxDuration, err := time.ParseDuration("30s")
	if err != nil {
		t.Fatalf("%s", err)
	}

	// First just validate the topics, don't create
	t.Logf("Validating topics before creation\n")
	ctx, cancel := context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	result, err := a.CreateTopics(ctx, newTopics,
		SetAdminValidateOnly(true))
	if err != nil {
		t.Fatalf("CreateTopics(ValidateOnly) failed: %s", err)
	}

	validateTopicResult(t, result, expError)

	// Now create the topics
	t.Logf("Creating topics\n")
	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	result, err = a.CreateTopics(ctx, newTopics, SetAdminValidateOnly(false))
	if err != nil {
		t.Fatalf("CreateTopics() failed: %s", err)
	}

	validateTopicResult(t, result, expError)

	// Attempt to create the topics again, should all fail.
	t.Logf("Attempt to re-create topics, should all fail\n")
	for k := range expError {
		if expError[k].code == ErrNoError {
			expError[k] = Error{code: ErrTopicAlreadyExists}
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	result, err = a.CreateTopics(ctx, newTopics)
	if err != nil {
		t.Fatalf("CreateTopics#2() failed: %s", err)
	}

	validateTopicResult(t, result, expError)

	// Add partitions to some of the topics
	t.Logf("Create new partitions for a subset of topics\n")
	newParts := make([]PartitionsSpecification, topicCnt/2)
	expError = map[string]Error{}
	for i := 0; i < topicCnt/2; i++ {
		topic := newTopics[i].Topic
		newParts[i] = PartitionsSpecification{
			Topic:      topic,
			IncreaseTo: newTopics[i].NumPartitions + 3,
		}
		if i == 1 {
			// Invalid partition count (less than current)
			newParts[i].IncreaseTo = newTopics[i].NumPartitions - 1
			expError[topic] = Error{code: ErrInvalidPartitions}
		} else {
			expError[topic] = Error{}
		}
		t.Logf("Creating new partitions for %s: %d -> %d: expecting %v\n",
			topic, newTopics[i].NumPartitions, newParts[i].IncreaseTo, expError[topic])
	}

	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	result, err = a.CreatePartitions(ctx, newParts)
	if err != nil {
		t.Fatalf("CreatePartitions() failed: %s", err)
	}

	validateTopicResult(t, result, expError)

	// FIXME: wait for topics to become available in metadata instead
	time.Sleep(5000 * time.Millisecond)

	// Delete the topics
	deleteTopics := make([]string, topicCnt)
	for i := 0; i < topicCnt; i++ {
		deleteTopics[i] = newTopics[i].Topic
		if i == topicCnt-1 {
			expError[deleteTopics[i]] = Error{code: ErrUnknownTopicOrPart}
		} else {
			expError[deleteTopics[i]] = Error{}
		}
	}

	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	result2, err := a.DeleteTopics(ctx, deleteTopics)
	if err != nil {
		t.Fatalf("DeleteTopics() failed: %s", err)
	}

	validateTopicResult(t, result2, expError)
}

func (its *IntegrationTestSuite) TestAdminConfig() {
	t := its.T()
	rand.Seed(time.Now().Unix())

	a := createAdminClient(t)
	defer a.Close()

	// Steps:
	//  1) Create a topic, providing initial non-default configuration
	//  2) Read back config to verify
	//  3) Alter config
	//  4) Read back config to verify
	//  5) Delete the topic

	topic := fmt.Sprintf("%s-config-%d", testconf.TopicName, rand.Intn(100000))

	// Expected config
	expResources := []ConfigResourceResult{
		{
			Type: ResourceTopic,
			Name: topic,
			Config: map[string]ConfigEntryResult{
				"compression.type": ConfigEntryResult{
					Name:  "compression.type",
					Value: "snappy",
				},
			},
		},
	}
	// Create topic
	newTopics := []TopicSpecification{{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
		Config:            map[string]string{"compression.type": "snappy"},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	topicResult, err := a.CreateTopics(ctx, newTopics)
	if err != nil {
		t.Fatalf("Create topic request failed: %v", err)
	}

	if topicResult[0].Error.Code() != ErrNoError {
		t.Fatalf("Failed to create topic %s: %s", topic, topicResult[0].Error)
	}

	// Wait for topic to show up in metadata before performing
	// subsequent operations on it, otherwise we risk DescribeConfigs()
	// to fail with UnknownTopic.. (this is really a broker issue).
	// Sometimes even the metadata is not enough, so we add an
	// arbitrary 10s sleep too.
	t.Logf("Waiting for new topic %s to show up in metadata and stabilize", topic)
	err = waitTopicInMetadata(a, topic, 10*1000) // 10s
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Logf("Topic %s now in metadata, waiting another 10s for stabilization", topic)
	time.Sleep(10 * time.Second)

	// Read back config to validate
	configResources := []ConfigResource{{Type: ResourceTopic, Name: topic}}
	describeRes, err := a.DescribeConfigs(ctx, configResources)
	if err != nil {
		t.Fatalf("Describe configs request failed: %v", err)
	}

	validateConfig(t, describeRes, expResources, true)

	// Alter some configs.
	// Configuration alterations are currently atomic, all values
	// need to be passed, otherwise non-passed values will be reverted
	// to their default values.
	newConfig := make(map[string]string)
	for _, entry := range describeRes[0].Config {
		newConfig[entry.Name] = entry.Value
	}

	// Change something
	newConfig["retention.ms"] = "86400000"
	newConfig["message.timestamp.type"] = "LogAppendTime"

	for k, v := range newConfig {
		expResources[0].Config[k] = ConfigEntryResult{Name: k, Value: v}
	}

	configResources = []ConfigResource{{Type: ResourceTopic, Name: topic, Config: StringMapToConfigEntries(newConfig, AlterOperationSet)}}
	alterRes, err := a.AlterConfigs(ctx, configResources)
	if err != nil {
		t.Fatalf("Alter configs request failed: %v", err)
	}

	validateConfig(t, alterRes, expResources, false)

	// Read back config to validate
	configResources = []ConfigResource{{Type: ResourceTopic, Name: topic}}
	describeRes, err = a.DescribeConfigs(ctx, configResources)
	if err != nil {
		t.Fatalf("Describe configs request failed: %v", err)
	}

	validateConfig(t, describeRes, expResources, true)

	// This is for incremental-alter.
	// We don't need to pass all configs. Just need to pass the configs
	// that are intended to change.
	newConfig = make(map[string]string)
	opsMap := make(map[string]AlterConfigOpType)

	// Change something
	newConfig["retention.ms"] = "86000000"
	opsMap["retention.ms"] = AlterConfigOpTypeSet
	// Default value for cleanup.policy(type=list) is delete
	newConfig["cleanup.policy"] = "compact"
	opsMap["cleanup.policy"] = AlterConfigOpTypeAppend
	newConfig["message.timestamp.type"] = ""
	// Default value for message.timestamp.type is CreateTime
	opsMap["message.timestamp.type"] = AlterConfigOpTypeDelete

	configResources = []ConfigResource{{Type: ResourceTopic, Name: topic,
		Config: StringMapToIncrementalConfigEntries(newConfig, opsMap)}}
	alterRes, err = a.IncrementalAlterConfigs(ctx, configResources)
	if err != nil {
		t.Fatalf("Incremental Alter Configs request failed: %v", err)
	}

	expResources[0].Config["retention.ms"] = ConfigEntryResult{Name: "retention.ms", Value: "86000000"}
	expResources[0].Config["cleanup.policy"] = ConfigEntryResult{Name: "cleanup.policy", Value: "delete,compact"}
	expResources[0].Config["message.timestamp.type"] = ConfigEntryResult{Name: "message.timestamp.type", Value: "CreateTime"}

	validateConfig(t, alterRes, expResources, false)

	// Read back config to validate
	configResources = []ConfigResource{{Type: ResourceTopic, Name: topic}}
	describeRes, err = a.DescribeConfigs(ctx, configResources)
	if err != nil {
		t.Fatalf("Describe configs request failed: %v", err)
	}

	validateConfig(t, describeRes, expResources, true)

	// Delete the topic
	// FIXME: wait for topics to become available in metadata instead
	time.Sleep(5000 * time.Millisecond)

	topicResult, err = a.DeleteTopics(ctx, []string{topic})
	if err != nil {
		t.Fatalf("DeleteTopics() failed: %s", err)
	}

	if topicResult[0].Error.Code() != ErrNoError {
		t.Fatalf("Failed to delete topic %s: %s", topic, topicResult[0].Error)
	}

	// TODO: enable this test for the classic run too, when
	// Confluent Platform test cluster is upgraded to 8.0.0
	if !testConsumerGroupProtocolClassic() {
		// Test configs that are configured for a specific group.
		groupID := fmt.Sprintf("test-group-%d", rand.Intn(100000))

		// Incremental Alter Configs for group
		t.Logf("Incrementally altering configs for consumer group %s", groupID)
		groupConfig := map[string]string{
			"consumer.session.timeout.ms": "50000",
		}
		groupOps := map[string]AlterConfigOpType{
			"consumer.session.timeout.ms": AlterConfigOpTypeSet,
		}

		groupConfigResource := ConfigResource{
			Type:   ResourceGroup,
			Name:   groupID,
			Config: StringMapToIncrementalConfigEntries(groupConfig, groupOps),
		}

		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()

		// Perform IncrementalAlterConfigs
		t.Logf("Performing IncrementalAlterConfigs for group %s", groupID)
		alterRes, err = a.IncrementalAlterConfigs(ctx, []ConfigResource{groupConfigResource})
		if err != nil {
			t.Fatalf("IncrementalAlterConfigs request failed for group %s: %v", groupID, err)
		}

		// Expected results
		expectedGroupConfig := []ConfigResourceResult{
			{
				Type: ResourceGroup,
				Name: groupID,
				Config: map[string]ConfigEntryResult{
					"consumer.session.timeout.ms": {
						Name:  "consumer.session.timeout.ms",
						Value: "50000",
					},
				},
			},
		}

		// Validate results from IncrementalAlterConfigs
		validateConfig(t, alterRes, expectedGroupConfig, false)

		// Read back group config to verify
		t.Logf("Describing configs for group %s", groupID)
		describeRes, err = a.DescribeConfigs(ctx, []ConfigResource{{Type: ResourceGroup, Name: groupID}})
		if err != nil {
			t.Fatalf("DescribeConfigs request failed for group %s: %v", groupID, err)
		}

		// Validate described configs
		validateConfig(t, describeRes, expectedGroupConfig, true)
	}
}

// Test AdminClient GetMetadata API
func (its *IntegrationTestSuite) TestAdminGetMetadata() {
	t := its.T()

	config := &ConfigMap{"bootstrap.servers": testconf.Brokers}
	config.updateFromTestconf()

	// Create Admin client
	a, err := NewAdminClient(config)
	if err != nil {
		t.Errorf("Failed to create Admin client: %s\n", err)
		return
	}
	defer a.Close()

	metaData, err := a.GetMetadata(&testconf.TopicName, false, 5*1000)
	if err != nil {
		t.Errorf("Failed to get meta data for topic %s. Error: %s\n", testconf.TopicName, err)
		return
	}
	t.Logf("Meta data for topic %s: %v\n", testconf.TopicName, metaData)

	metaData, err = a.GetMetadata(nil, true, 5*1000)
	if err != nil {
		t.Errorf("Failed to get meta data, Error: %s\n", err)
		return
	}
	t.Logf("Meta data for admin client: %v\n", metaData)
}

// Test AdminClient ClusterID.
func (its *IntegrationTestSuite) TestAdminClient_ClusterID() {
	t := its.T()

	config := &ConfigMap{"bootstrap.servers": testconf.Brokers}
	if err := config.updateFromTestconf(); err != nil {
		t.Fatalf("Failed to update test configuration: %s\n", err)
	}

	admin, err := NewAdminClient(config)
	if err != nil {
		t.Fatalf("Failed to create Admin client: %s\n", err)
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clusterID, err := admin.ClusterID(ctx)
	if err != nil {
		t.Fatalf("Failed to get ClusterID: %s\n", err)
	}
	if clusterID == "" {
		t.Fatal("ClusterID is empty.")
	}

	t.Logf("ClusterID: %s\n", clusterID)
}

// Test AdminClient ControllerID.
func (its *IntegrationTestSuite) TestAdminClient_ControllerID() {
	t := its.T()

	config := &ConfigMap{"bootstrap.servers": testconf.Brokers}
	if err := config.updateFromTestconf(); err != nil {
		t.Fatalf("Failed to update test configuration: %s\n", err)
	}

	producer, err := NewProducer(config)
	if err != nil {
		t.Fatalf("Failed to create Producer client: %s\n", err)
	}
	admin, err := NewAdminClientFromProducer(producer)
	if err != nil {
		t.Fatalf("Failed to create Admin client: %s\n", err)
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	controllerID, err := admin.ControllerID(ctx)
	if err != nil {
		t.Fatalf("Failed to get ControllerID: %s\n", err)
	}
	if controllerID < 0 {
		t.Fatalf("ControllerID is negative: %d\n", controllerID)
	}

	t.Logf("ControllerID: %d\n", controllerID)
}

func (its *IntegrationTestSuite) TestAdminACLs() {
	t := its.T()

	rand.Seed(time.Now().Unix())
	topic := testconf.TopicName
	group := testconf.GroupID
	noError := NewError(ErrNoError, "", false)
	var expectedCreateACLs []CreateACLResult
	var expectedDescribeACLs DescribeACLsResult
	var expectedDeleteACLs []DeleteACLsResult
	var ctx context.Context
	var cancel context.CancelFunc

	a := createAdminClient(t)
	defer a.Close()

	maxDuration, err := time.ParseDuration("30s")
	if err != nil {
		t.Fatalf("%s", err)
	}
	requestTimeout, err := time.ParseDuration("20s")
	if err != nil {
		t.Fatalf("%s", err)
	}

	checkExpectedResult := func(expected interface{}, result interface{}) {
		if !reflect.DeepEqual(result, expected) {
			t.Fatalf("Expected result to deep equal to %v, but found %v", expected, result)
		}
	}

	// Create ACLs
	t.Logf("Creating ACLs\n")
	newACLs := ACLBindings{
		{
			Type:                ResourceTopic,
			Name:                topic,
			ResourcePatternType: ResourcePatternTypeLiteral,
			Principal:           "User:test-user-1",
			Host:                "*",
			Operation:           ACLOperationRead,
			PermissionType:      ACLPermissionTypeAllow,
		},
		{
			Type:                ResourceTopic,
			Name:                topic,
			ResourcePatternType: ResourcePatternTypePrefixed,
			Principal:           "User:test-user-2",
			Host:                "*",
			Operation:           ACLOperationWrite,
			PermissionType:      ACLPermissionTypeDeny,
		},
		{
			Type:                ResourceGroup,
			Name:                group,
			ResourcePatternType: ResourcePatternTypePrefixed,
			Principal:           "User:test-user-2",
			Host:                "*",
			Operation:           ACLOperationAll,
			PermissionType:      ACLPermissionTypeAllow,
		},
	}

	invalidACLs := ACLBindings{
		{
			Type:                ResourceTopic,
			Name:                topic,
			ResourcePatternType: ResourcePatternTypeLiteral,
			// Principal must be in the form "{principalType}:{principalName}"
			// Broker returns ErrUnknown in this case
			Principal:      "wrong-principal",
			Host:           "*",
			Operation:      ACLOperationRead,
			PermissionType: ACLPermissionTypeAllow,
		},
	}

	aclBindingFilters := ACLBindingFilters{
		{
			Type:                ResourceAny,
			ResourcePatternType: ResourcePatternTypeAny,
			Operation:           ACLOperationAny,
			PermissionType:      ACLPermissionTypeAny,
		},
		{
			Type:                ResourceAny,
			ResourcePatternType: ResourcePatternTypePrefixed,
			Operation:           ACLOperationAny,
			PermissionType:      ACLPermissionTypeAny,
		},
		{
			Type:                ResourceTopic,
			ResourcePatternType: ResourcePatternTypeAny,
			Operation:           ACLOperationAny,
			PermissionType:      ACLPermissionTypeAny,
		},
		{
			Type:                ResourceGroup,
			ResourcePatternType: ResourcePatternTypeAny,
			Operation:           ACLOperationAny,
			PermissionType:      ACLPermissionTypeAny,
		},
	}

	// CreateACLs should be idempotent
	for n := 0; n < 2; n++ {
		ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
		defer cancel()

		resultCreateACLs, err := a.CreateACLs(ctx, newACLs, SetAdminRequestTimeout(requestTimeout))
		if err != nil {
			t.Fatalf("CreateACLs() failed: %s", err)
		}
		expectedCreateACLs = []CreateACLResult{{Error: noError}, {Error: noError}, {Error: noError}}
		checkExpectedResult(expectedCreateACLs, resultCreateACLs)
	}

	// CreateACLs with server side validation errors
	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()

	resultCreateACLs, err := a.CreateACLs(ctx, invalidACLs, SetAdminRequestTimeout(requestTimeout))
	if err != nil {
		t.Fatalf("CreateACLs() failed: %s", err)
	}
	t.Logf("CreateACLs result: %v", resultCreateACLs[0].Error.Code())

	if len(resultCreateACLs) != 1 {
		t.Fatalf("CreateACLs result should contain one result")
	}

	// Depending on Kraft/ZK, the error may be ErrUnknown or ErrInvalidRequest.
	if resultCreateACLs[0].Error.Code() != ErrUnknown &&
		resultCreateACLs[0].Error.Code() != ErrInvalidRequest {
		t.Fatalf("CreateACLs result should have an error of either ErrUnknown or ErrInvalidRequest, is %v",
			resultCreateACLs[0].Error.Code())
	}

	// DescribeACLs must return the three ACLs
	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	resultDescribeACLs, err := a.DescribeACLs(ctx, aclBindingFilters[0], SetAdminRequestTimeout(requestTimeout))
	expectedDescribeACLs = DescribeACLsResult{
		Error:       noError,
		ACLBindings: newACLs,
	}
	if err != nil {
		t.Fatalf("%s", err)
	}
	sort.Sort(&resultDescribeACLs.ACLBindings)
	checkExpectedResult(expectedDescribeACLs, *resultDescribeACLs)

	// Delete the ACLs with ResourcePatternTypePrefixed
	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	resultDeleteACLs, err := a.DeleteACLs(ctx, aclBindingFilters[1:2], SetAdminRequestTimeout(requestTimeout))
	expectedDeleteACLs = []DeleteACLsResult{
		{
			Error:       noError,
			ACLBindings: newACLs[1:3],
		},
	}
	if err != nil {
		t.Fatalf("%s", err)
	}
	sort.Sort(&resultDeleteACLs[0].ACLBindings)
	checkExpectedResult(expectedDeleteACLs, resultDeleteACLs)

	// Delete the ACLs with ResourceTopic and ResourceGroup
	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	resultDeleteACLs, err = a.DeleteACLs(ctx, aclBindingFilters[2:4], SetAdminRequestTimeout(requestTimeout))
	expectedDeleteACLs = []DeleteACLsResult{
		{
			Error:       noError,
			ACLBindings: newACLs[0:1],
		},
		{
			Error:       noError,
			ACLBindings: ACLBindings{},
		},
	}
	if err != nil {
		t.Fatalf("%s", err)
	}
	checkExpectedResult(expectedDeleteACLs, resultDeleteACLs)

	// All the ACLs should have been deleted
	ctx, cancel = context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	resultDescribeACLs, err = a.DescribeACLs(ctx, aclBindingFilters[0], SetAdminRequestTimeout(requestTimeout))
	expectedDescribeACLs = DescribeACLsResult{
		Error:       noError,
		ACLBindings: ACLBindings{},
	}
	if err != nil {
		t.Fatalf("%s", err)
	}
	checkExpectedResult(expectedDescribeACLs, *resultDescribeACLs)
}

// Test AdminClient List all consumer group offsets.
func (its *IntegrationTestSuite) TestAdminClient_ListAllConsumerGroupsOffsets() {
	t := its.T()
	rand.Seed(time.Now().Unix())

	conf := &ConfigMap{"bootstrap.servers": testconf.Brokers}
	if err := conf.updateFromTestconf(); err != nil {
		t.Fatalf("Failed to update test configuration: %s\n", err)
	}

	admin, err := NewAdminClient(conf)
	if err != nil {
		t.Fatalf("Failed to create Admin client: %s\n", err)
	}
	defer admin.Close()

	// Create some topics.
	numTopics := 3
	topics := make([]string, 0)
	topicSpec := make([]TopicSpecification, 0)

	for i := 0; i < numTopics; i++ {
		topic := fmt.Sprintf("%s-%d", testconf.TopicName, rand.Intn(100000))
		topics = append(topics, topic)
		topicSpec = append(
			topicSpec, TopicSpecification{Topic: topic, NumPartitions: i + 1})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	topicResult, err := admin.CreateTopics(ctx, topicSpec)

	if err != nil {
		t.Fatalf("Failed to create topics: %s\n", err)
	}

	for i := 0; i < numTopics; i++ {
		if topicResult[i].Error.Code() != ErrNoError {
			t.Fatalf("Failed to create topic %s: %s", topicSpec[i].Topic, topicResult[i].Error)
		}
	}

	// Join a consumer group and subscribe to the created topics,
	// commit some offsets to read later.
	group := fmt.Sprintf("%s-%d", testconf.GroupID, rand.Intn(100000))
	conf = &ConfigMap{
		"bootstrap.servers": testconf.Brokers,
		"group.id":          group,
		"auto.offset.reset": "end",
	}
	conf.updateFromTestconf()

	consumer, err := testNewConsumer(t, conf)
	if err != nil {
		t.Fatalf("Failed to create consumer: %s\n", err)
	}
	defer consumer.Close()

	if err = consumer.SubscribeTopics(topics, nil); err != nil {
		t.Fatalf("Failed to subscribe to topics: %s\n", err)
	}

	// Poll for a while, wait for rebalance.
	consumer.Poll(10000)

	for i := 0; i < numTopics; i++ {
		for j := 0; j < topicSpec[i].NumPartitions; j++ {
			_, err = consumer.CommitOffsets([]TopicPartition{
				{
					Topic:     &topics[i],
					Partition: int32(j),
					Offset:    Offset((i * topicSpec[i].NumPartitions) + j),
				},
			})
			if err != nil {
				t.Fatalf("Could not commit for %s:%d: %s\n", topics[i], j, err)
			}
		}
	}

	// List consumer group offsets.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	offsets, err := admin.ListConsumerGroupOffsets(
		ctx,
		[]ConsumerGroupTopicPartitions{{Group: group}},
		SetAdminRequireStableOffsets(true))

	if err != nil {
		t.Fatalf("Failed to get consumer group offsets: %s\n", err)
	}

	if len(offsets.ConsumerGroupsTopicPartitions) != 1 {
		t.Fatal("Consumer group offsets is empty")
	}

	toppars := offsets.ConsumerGroupsTopicPartitions[0].Partitions

	// Use linear search - okay for small numTopics. Since the returned list
	// is not ordered in any particular way.
	matchedToppars := 0
	for i := 0; i < numTopics; i++ {
		for k := 0; k < len(toppars); k++ {
			if topics[i] != *toppars[k].Topic {
				continue
			}
			matchedToppars++
			partition := int(toppars[k].Partition)
			expectedOffset := i*topicSpec[i].NumPartitions + partition

			if toppars[k].Offset != Offset(expectedOffset) {
				t.Fatalf("Expected offset %d for %s:%d, got %d\n",
					expectedOffset, topics[i], partition, toppars[k].Offset)
			}
		}
	}

	totalPartitions := ((numTopics + 1) * numTopics) / 2
	if matchedToppars != totalPartitions {
		t.Fatalf("Expected to match %d total topic partitions, matched %d\n",
			totalPartitions, matchedToppars)
	}
}

// Test consumer QueryWatermarkOffsets API
func (its *IntegrationTestSuite) TestConsumerQueryWatermarkOffsets() {
	t := its.T()

	// getMessageCountInTopic() uses consumer QueryWatermarkOffsets() API to
	// get the number of messages in a topic
	msgcnt, err := getMessageCountInTopic(testconf.TopicName)
	if err != nil {
		t.Errorf("Cannot get message size. Error: %s\n", err)
	}

	// Prime topic with test messages
	createTestMessages()
	producerTest(t, "Priming producer", p0TestMsgs, producerCtrl{silent: true},
		func(p *Producer, m *Message, drChan chan Event) {
			p.ProduceChannel() <- m
		})

	// getMessageCountInTopic() uses consumer QueryWatermarkOffsets() API to
	// get the number of messages in a topic
	newmsgcnt, err := getMessageCountInTopic(testconf.TopicName)
	if err != nil {
		t.Errorf("Cannot get message size. Error: %s\n", err)
	}

	if newmsgcnt-msgcnt != len(p0TestMsgs) {
		t.Errorf("Incorrect offsets. Expected message count %d, got %d\n", len(p0TestMsgs), newmsgcnt-msgcnt)
	}

}

// Test consumer GetWatermarkOffsets API
func (its *IntegrationTestSuite) TestConsumerGetWatermarkOffsets() {
	t := its.T()

	// Create consumer
	config := &ConfigMap{
		"go.events.channel.enable": true,
		"bootstrap.servers":        testconf.Brokers,
		"group.id":                 testconf.GroupID,
		"session.timeout.ms":       6000,
		"enable.auto.commit":       false,
		"auto.offset.reset":        "earliest",
	}
	_ = config.updateFromTestconf()

	c, err := testNewConsumer(t, config)
	if err != nil {
		t.Fatalf("Unable to create consumer: %s", err)
	}
	defer func() { _ = c.Close() }()

	err = c.Subscribe(testconf.TopicName, nil)

	// Prime topic with test messages
	createTestMessages()
	producerTest(t, "Priming producer", p0TestMsgs, producerCtrl{silent: true},
		func(p *Producer, m *Message, drChan chan Event) {
			p.ProduceChannel() <- m
		})

	// Wait for messages to be received so that we know the watermark offsets have been delivered
	// with the fetch response
	for ev := range c.Events() {
		if _, ok := ev.(*Message); ok {
			break
		}
	}

	_, queryHigh, err := c.QueryWatermarkOffsets(testconf.TopicName, 0, 5*1000)
	if err != nil {
		t.Fatalf("Error querying watermark offsets: %s", err)
	}

	// We are not currently testing the low watermark offset as it only gets set every 10s by the stits timer
	_, getHigh, err := c.GetWatermarkOffsets(testconf.TopicName, 0)
	if err != nil {
		t.Fatalf("Error getting watermark offsets: %s", err)
	}

	if queryHigh != getHigh {
		t.Errorf("QueryWatermarkOffsets high[%d] does not equal GetWatermarkOffsets high[%d]", queryHigh, getHigh)
	}

}

// TestConsumerOffsetsForTimes
func (its *IntegrationTestSuite) TestConsumerOffsetsForTimes() {
	t := its.T()

	conf := ConfigMap{"bootstrap.servers": testconf.Brokers,
		"group.id":            testconf.GroupID,
		"api.version.request": true}

	conf.updateFromTestconf()

	c, err := testNewConsumer(t, &conf)

	if err != nil {
		panic(err)
	}
	defer c.Close()

	// Prime topic with test messages
	createTestMessages()
	producerTest(t, "Priming producer", p0TestMsgs, producerCtrl{silent: true},
		func(p *Producer, m *Message, drChan chan Event) {
			p.ProduceChannel() <- m
		})

	times := make([]TopicPartition, 1)
	times[0] = TopicPartition{Topic: &testconf.TopicName, Partition: 0, Offset: 12345}
	offsets, err := c.OffsetsForTimes(times, 5000)
	if err != nil {
		t.Errorf("OffsetsForTimes() failed: %s\n", err)
		return
	}

	if len(offsets) != 1 {
		t.Errorf("OffsetsForTimes() returned wrong length %d, expected 1\n", len(offsets))
		return
	}

	if *offsets[0].Topic != testconf.TopicName || offsets[0].Partition != 0 {
		t.Errorf("OffsetsForTimes() returned wrong topic/partition\n")
		return
	}

	if offsets[0].Error != nil {
		t.Errorf("OffsetsForTimes() returned error for partition 0: %s\n", err)
		return
	}

	low, _, err := c.QueryWatermarkOffsets(testconf.TopicName, 0, 5*1000)
	if err != nil {
		t.Errorf("Failed to query watermark offsets for topic %s. Error: %s\n", testconf.TopicName, err)
		return
	}

	t.Logf("OffsetsForTimes() returned offset %d for timestamp %d\n", offsets[0].Offset, times[0].Offset)

	// Since we're using a phony low timestamp it is assumed that the returned
	// offset will be oldest message.
	if offsets[0].Offset != Offset(low) {
		t.Errorf("OffsetsForTimes() returned invalid offset %d for timestamp %d, expected %d\n", offsets[0].Offset, times[0].Offset, low)
		return
	}

}

// test consumer GetMetadata API
func (its *IntegrationTestSuite) TestConsumerGetMetadata() {
	t := its.T()

	config := &ConfigMap{"bootstrap.servers": testconf.Brokers,
		"group.id": testconf.GroupID}
	config.updateFromTestconf()

	// Create consumer
	c, err := testNewConsumer(t, config)
	if err != nil {
		t.Errorf("Failed to create consumer: %s\n", err)
		return
	}
	defer c.Close()

	metaData, err := c.GetMetadata(&testconf.TopicName, false, 5*1000)
	if err != nil {
		t.Errorf("Failed to get meta data for topic %s. Error: %s\n", testconf.TopicName, err)
		return
	}
	t.Logf("Meta data for topic %s: %v\n", testconf.TopicName, metaData)

	metaData, err = c.GetMetadata(nil, true, 5*1000)
	if err != nil {
		t.Errorf("Failed to get meta data, Error: %s\n", err)
		return
	}
	t.Logf("Meta data for consumer: %v\n", metaData)
}

// Test producer QueryWatermarkOffsets API
func (its *IntegrationTestSuite) TestProducerQueryWatermarkOffsets() {
	t := its.T()

	config := &ConfigMap{"bootstrap.servers": testconf.Brokers}
	config.updateFromTestconf()

	// Create producer
	p, err := NewProducer(config)
	if err != nil {
		t.Errorf("Failed to create producer: %s\n", err)
		return
	}
	defer p.Close()

	low, high, err := p.QueryWatermarkOffsets(testconf.TopicName, 0, 5*1000)
	if err != nil {
		t.Errorf("Failed to query watermark offsets for topic %s. Error: %s\n", testconf.TopicName, err)
		return
	}
	cnt := high - low
	t.Logf("Watermark offsets fo topic %s: low=%d, high=%d\n", testconf.TopicName, low, high)

	createTestMessages()
	producerTest(t, "Priming producer", p0TestMsgs, producerCtrl{silent: true},
		func(p *Producer, m *Message, drChan chan Event) {
			p.ProduceChannel() <- m
		})

	low, high, err = p.QueryWatermarkOffsets(testconf.TopicName, 0, 5*1000)
	if err != nil {
		t.Errorf("Failed to query watermark offsets for topic %s. Error: %s\n", testconf.TopicName, err)
		return
	}
	t.Logf("Watermark offsets fo topic %s: low=%d, high=%d\n", testconf.TopicName, low, high)
	newcnt := high - low
	t.Logf("count = %d, New count = %d\n", cnt, newcnt)
	if newcnt-cnt != int64(len(p0TestMsgs)) {
		t.Errorf("Incorrect offsets. Expected message count %d, got %d\n", len(p0TestMsgs), newcnt-cnt)
	}
}

// Test producer GetMetadata API
func (its *IntegrationTestSuite) TestProducerGetMetadata() {
	t := its.T()

	config := &ConfigMap{"bootstrap.servers": testconf.Brokers}
	config.updateFromTestconf()

	// Create producer
	p, err := NewProducer(config)
	if err != nil {
		t.Errorf("Failed to create producer: %s\n", err)
		return
	}
	defer p.Close()

	metaData, err := p.GetMetadata(&testconf.TopicName, false, 5*1000)
	if err != nil {
		t.Errorf("Failed to get meta data for topic %s. Error: %s\n", testconf.TopicName, err)
		return
	}
	t.Logf("Meta data for topic %s: %v\n", testconf.TopicName, metaData)

	metaData, err = p.GetMetadata(nil, true, 5*1000)
	if err != nil {
		t.Errorf("Failed to get meta data, Error: %s\n", err)
		return
	}
	t.Logf("Meta data for producer: %v\n", metaData)

}

// test producer function-based API without delivery report
func (its *IntegrationTestSuite) TestProducerFunc() {
	t := its.T()
	producerTest(t, "Function producer (without DR)",
		nil, producerCtrl{},
		func(p *Producer, m *Message, drChan chan Event) {
			err := p.Produce(m, drChan)
			if err != nil {
				t.Errorf("Produce() failed: %v", err)
			}
		})
}

// test producer function-based API with delivery report
func (its *IntegrationTestSuite) TestProducerFuncDR() {
	t := its.T()
	producerTest(t, "Function producer (with DR)",
		nil, producerCtrl{withDr: true},
		func(p *Producer, m *Message, drChan chan Event) {
			err := p.Produce(m, drChan)
			if err != nil {
				t.Errorf("Produce() failed: %v", err)
			}
		})
}

// test producer with bad messages
func (its *IntegrationTestSuite) TestProducerWithBadMessages() {
	t := its.T()
	conf := ConfigMap{"bootstrap.servers": testconf.Brokers}
	conf.updateFromTestconf()

	p, err := NewProducer(&conf)
	if err != nil {
		panic(err)
	}
	defer p.Close()

	// producing a nil message should return an error without crash
	err = p.Produce(nil, p.Events())
	if err == nil {
		t.Errorf("Producing a nil message should return error\n")
	} else {
		t.Logf("Producing a nil message returns expected error: %s\n", err)
	}

	// producing a blank message (with nil Topic) should return an error without crash
	err = p.Produce(&Message{}, p.Events())
	if err == nil {
		t.Errorf("Producing a blank message should return error\n")
	} else {
		t.Logf("Producing a blank message returns expected error: %s\n", err)
	}
}

// test producer channel-based API without delivery report
func (its *IntegrationTestSuite) TestProducerChannel() {
	t := its.T()
	producerTest(t, "Channel producer (without DR)",
		nil, producerCtrl{},
		func(p *Producer, m *Message, drChan chan Event) {
			p.ProduceChannel() <- m
		})
}

// test producer channel-based API with delivery report
func (its *IntegrationTestSuite) TestProducerChannelDR() {
	t := its.T()
	producerTest(t, "Channel producer (with DR)",
		nil, producerCtrl{withDr: true},
		func(p *Producer, m *Message, drChan chan Event) {
			p.ProduceChannel() <- m
		})

}

// test batch producer channel-based API without delivery report
func (its *IntegrationTestSuite) TestProducerBatchChannel() {
	t := its.T()
	producerTest(t, "Channel producer (without DR, batch channel)",
		nil, producerCtrl{batchProducer: true},
		func(p *Producer, m *Message, drChan chan Event) {
			p.ProduceChannel() <- m
		})
}

// test batch producer channel-based API with delivery report
func (its *IntegrationTestSuite) TestProducerBatchChannelDR() {
	t := its.T()
	producerTest(t, "Channel producer (DR, batch channel)",
		nil, producerCtrl{withDr: true, batchProducer: true},
		func(p *Producer, m *Message, drChan chan Event) {
			p.ProduceChannel() <- m
		})
}

// test consumer channel-based API
func (its *IntegrationTestSuite) TestConsumerChannel() {
	t := its.T()
	consumerTestWithCommits(t, "Channel Consumer",
		"", 0, true, eventTestChannelConsumer, nil)
}

// test consumer channel-based API with incremental rebalancing
func (its *IntegrationTestSuite) TestConsumerChannelIncremental() {
	t := its.T()
	consumerTestWithCommits(t, "Channel Consumer Incremental",
		"cooperative-sticky", 0, true, eventTestChannelConsumer, nil)
}

// test consumer poll-based API
func (its *IntegrationTestSuite) TestConsumerPoll() {
	t := its.T()
	consumerTestWithCommits(t, "Poll Consumer", "", 0, false, eventTestPollConsumer, nil)
}

// test consumer poll-based API with incremental rebalancing
func (its *IntegrationTestSuite) TestConsumerPollIncremental() {
	t := its.T()
	consumerTestWithCommits(t, "Poll Consumer incremental",
		"cooperative-sticky", 0, false, eventTestPollConsumer, nil)
}

// test consumer poll-based API with rebalance callback
func (its *IntegrationTestSuite) TestConsumerPollRebalance() {
	t := its.T()
	consumerTestWithCommits(t, "Poll Consumer (rebalance callback)",
		"", 0, false, eventTestPollConsumer,
		func(c *Consumer, event Event) error {
			t.Logf("Rebalanced: %s", event)
			return nil
		})
}

// test consumer poll-based API with incremental no-op rebalance callback
func (its *IntegrationTestSuite) TestConsumerPollRebalanceIncrementalNoop() {
	t := its.T()
	consumerTestWithCommits(t, "Poll Consumer (incremental no-op rebalance callback)",
		"cooperative-sticky", 0, false, eventTestPollConsumer,
		func(c *Consumer, event Event) error {
			t.Logf("Rebalanced: %s", event)
			return nil
		})
}

// test consumer poll-based API with incremental rebalance callback
func (its *IntegrationTestSuite) TestConsumerPollRebalanceIncremental() {
	t := its.T()
	consumerTestWithCommits(t, "Poll Consumer (incremental rebalance callback)",
		"cooperative-sticky", 0, false, eventTestPollConsumer,
		func(c *Consumer, event Event) error {
			t.Logf("Rebalanced: %s (RebalanceProtocol=%s, AssignmentLost=%v)",
				event, c.GetRebalanceProtocol(), c.AssignmentLost())

			switch e := event.(type) {
			case AssignedPartitions:
				err := c.IncrementalAssign(e.Partitions)
				if err != nil {
					t.Errorf("IncrementalAssign() failed: %s\n", err)
					return err
				}
			case RevokedPartitions:
				err := c.IncrementalUnassign(e.Partitions)
				if err != nil {
					t.Errorf("IncrementalUnassign() failed: %s\n", err)
					return err
				}
			default:
				t.Fatalf("Unexpected rebalance event: %v\n", e)
			}

			return nil
		})
}

// Test Committed() API
func (its *IntegrationTestSuite) TestConsumerCommitted() {
	t := its.T()

	consumerTestWithCommits(t, "Poll Consumer (rebalance callback, verify Committed())",
		"", 0, false, eventTestPollConsumer,
		func(c *Consumer, event Event) error {
			t.Logf("Rebalanced: %s", event)
			rp, ok := event.(RevokedPartitions)
			if ok {
				offsets, err := c.Committed(rp.Partitions, 5000)
				if err != nil {
					t.Errorf("Failed to get committed offsets: %s\n", err)
					return nil
				}

				t.Logf("Retrieved Committed offsets: %s\n", offsets)

				if len(offsets) != len(rp.Partitions) || len(rp.Partitions) == 0 {
					t.Errorf("Invalid number of partitions %d, should be %d (and >0)\n", len(offsets), len(rp.Partitions))
				}

				// Verify proper offsets: at least one partition needs
				// to have a committed offset.
				validCnt := 0
				for _, p := range offsets {
					if p.Error != nil {
						t.Errorf("Committed() partition error: %v: %v", p, p.Error)
					} else if p.Offset >= 0 {
						validCnt++
					}
				}

				if validCnt == 0 {
					t.Errorf("Committed(): no partitions with valid offsets: %v", offsets)
				}
			}
			return nil
		})
}

// TestProducerConsumerTimestamps produces messages with timestamps
// and verifies them on consumption.
// Requires librdkafka >=0.9.4 and Kafka >=0.10.0.0
func (its *IntegrationTestSuite) TestProducerConsumerTimestamps() {
	t := its.T()
	numver, strver := LibraryVersion()
	if numver < 0x00090400 {
		t.Skipf("Requires librdkafka >=0.9.4 (currently on %s)", strver)
	}

	// Create topic if it doesn't exist
	topic := createTestTopic(t, "TestProducerConsumerTimestamps", 1, 1)

	consumerConf := ConfigMap{"bootstrap.servers": testconf.Brokers,
		"go.events.channel.enable": true,
		"group.id":                 topic,
		"enable.partition.eof":     true,
	}

	consumerConf.updateFromTestconf()

	/* Create consumer and find recognizable message, verify timestamp.
	 * The consumer is started before the producer to make sure
	 * the message isn't missed. */
	t.Logf("Creating consumer")
	c, err := testNewConsumer(t, &consumerConf)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	t.Logf("Assign %s [0]", topic)
	err = c.Assign([]TopicPartition{{Topic: &topic, Partition: 0,
		Offset: OffsetEnd}})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}

	/* Wait until EOF is reached so we dont miss the produced message */
	for ev := range c.Events() {
		t.Logf("Awaiting initial EOF")
		_, ok := ev.(PartitionEOF)
		if ok {
			break
		}
	}

	/*
	 * Create producer and produce one recognizable message with timestamp
	 */
	producerConf := ConfigMap{"bootstrap.servers": testconf.Brokers}
	producerConf.updateFromTestconf()

	t.Logf("Creating producer")
	p, err := NewProducer(&producerConf)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	drChan := make(chan Event, 1)

	/* Offset the timestamp to avoid comparison with system clock */
	future, _ := time.ParseDuration("1h") // 1h
	timestamp := time.Now().Add(future)
	key := fmt.Sprintf("TS: %v", timestamp)
	t.Logf("Producing message with timestamp %v", timestamp)
	err = p.Produce(&Message{
		TopicPartition: TopicPartition{Topic: &topic, Partition: 0},
		Key:            []byte(key),
		Timestamp:      timestamp},
		drChan)

	if err != nil {
		t.Fatalf("Produce: %v", err)
	}

	// Wait for delivery
	t.Logf("Awaiting delivery report")
	ev := <-drChan
	m, ok := ev.(*Message)
	if !ok {
		t.Fatalf("drChan: Expected *Message, got %v", ev)
	}
	if m.TopicPartition.Error != nil {
		t.Fatalf("Delivery failed: %v", m.TopicPartition)
	}
	t.Logf("Produced message to %v", m.TopicPartition)
	producedOffset := m.TopicPartition.Offset

	p.Close()

	/* Now consume messages, waiting for that recognizable one. */
	t.Logf("Consuming messages")
outer:
	for ev := range c.Events() {
		switch m := ev.(type) {
		case *Message:
			if m.TopicPartition.Error != nil {
				continue
			}
			if m.Key == nil || string(m.Key) != key {
				continue
			}

			t.Logf("Found message at %v with timestamp %s %s",
				m.TopicPartition,
				m.TimestampType, m.Timestamp)

			if m.TopicPartition.Offset != producedOffset {
				t.Fatalf("Produced Offset %d does not match consumed offset %d", producedOffset, m.TopicPartition.Offset)
			}

			if m.TimestampType != TimestampCreateTime {
				t.Fatalf("Expected timestamp CreateTime, not %s",
					m.TimestampType)
			}

			/* Since Kafka timestamps are milliseconds we need to
			 * shave off some precision for the comparison */
			if m.Timestamp.UnixNano()/1000000 !=
				timestamp.UnixNano()/1000000 {
				t.Fatalf("Expected timestamp %v (%d), not %v (%d)",
					timestamp, timestamp.UnixNano(),
					m.Timestamp, m.Timestamp.UnixNano())
			}
			break outer
		default:
		}
	}

	c.Close()
}

// TestProducerConsumerHeaders produces messages with headers
// and verifies them on consumption.
// Requires librdkafka >=0.11.4 and Kafka >=0.11.0.0
func (its *IntegrationTestSuite) TestProducerConsumerHeaders() {
	t := its.T()
	numver, strver := LibraryVersion()
	if numver < 0x000b0400 {
		t.Skipf("Requires librdkafka >=0.11.4 (currently on %s, 0x%x)", strver, numver)
	}

	conf := ConfigMap{"bootstrap.servers": testconf.Brokers,
		"api.version.request": true,
		"enable.auto.commit":  false,
		"group.id":            testconf.TopicName,
	}

	conf.updateFromTestconf()

	/*
	 * Create producer and produce a couple of messages with and without
	 * headers.
	 */
	t.Logf("Creating producer")
	p, err := NewProducer(&conf)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	drChan := make(chan Event, 1)

	// prepare some header values
	bigBytes := make([]byte, 2500)
	for i := 0; i < len(bigBytes); i++ {
		bigBytes[i] = byte(i)
	}

	myVarint := make([]byte, binary.MaxVarintLen64)
	myVarintLen := binary.PutVarint(myVarint, 12345678901234)

	expMsgHeaders := [][]Header{
		{
			{"msgid", []byte("1")},
			{"a key with SPACES ", bigBytes[:15]},
			{"BIGONE!", bigBytes},
		},
		{
			{"msgid", []byte("2")},
			{"myVarint", myVarint[:myVarintLen]},
			{"empty", []byte("")},
			{"theNullIsNil", nil},
		},
		nil, // no headers
		{
			{"msgid", []byte("4")},
			{"order", []byte("1")},
			{"order", []byte("2")},
			{"order", nil},
			{"order", []byte("4")},
		},
	}

	t.Logf("Producing %d messages", len(expMsgHeaders))
	for _, hdrs := range expMsgHeaders {
		err = p.Produce(&Message{
			TopicPartition: TopicPartition{Topic: &testconf.TopicName, Partition: 0},
			Headers:        hdrs},
			drChan)
	}

	if err != nil {
		t.Fatalf("Produce: %v", err)
	}

	var firstOffset Offset = OffsetInvalid
	for range expMsgHeaders {
		ev := <-drChan
		m, ok := ev.(*Message)
		if !ok {
			t.Fatalf("drChan: Expected *Message, got %v", ev)
		}
		if m.TopicPartition.Error != nil {
			t.Fatalf("Delivery failed: %v", m.TopicPartition)
		}
		t.Logf("Produced message to %v", m.TopicPartition)
		if firstOffset == OffsetInvalid {
			firstOffset = m.TopicPartition.Offset
		}
	}

	p.Close()

	/* Now consume the produced messages and verify the headers */
	t.Logf("Creating consumer starting at offset %v", firstOffset)
	c, err := testNewConsumer(t, &conf)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	err = c.Assign([]TopicPartition{{Topic: &testconf.TopicName, Partition: 0,
		Offset: firstOffset}})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}

	for n, hdrs := range expMsgHeaders {
		m, err := c.ReadMessage(-1)
		if err != nil {
			t.Fatalf("Expected message #%d, not error %v", n, err)
		}

		if m.Headers == nil {
			if hdrs == nil {
				continue
			}
			t.Fatalf("Expected message #%d to have headers", n)
		}

		if hdrs == nil {
			t.Fatalf("Expected message #%d not to have headers, but found %v", n, m.Headers)
		}

		// Compare headers
		if !reflect.DeepEqual(hdrs, m.Headers) {
			t.Fatalf("Expected message #%d headers to match %v, but found %v", n, hdrs, m.Headers)
		}

		t.Logf("Message #%d headers matched: %v", n, m.Headers)
	}

	c.Close()

}

// TestUserScramTestAdminClient_UserScramCredentialsCredentialsAPI describes
// the SCRAM credentials for a user, upserts some credentials, describes them
// again to check insertion, deletes them, and finally describes them once again
// to check deletion.
func (its *IntegrationTestSuite) TestAdminClient_UserScramCredentials() {
	t := its.T()
	ac, err := NewAdminClient(&ConfigMap{
		"bootstrap.servers": testconf.Brokers,
	})
	if err != nil {
		t.Fatalf("Failed to create Admin Client: %s\n", err)
	}
	defer ac.Close()

	users := []string{"non-existent"}

	// Call DescribeUserScramCredentials
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	describeRes, describeErr := ac.DescribeUserScramCredentials(ctx, users)
	if describeErr != nil {
		t.Fatalf("Failed to Describe the User Scram Credentials: %s\n", describeErr)
	}

	// Check Describe result
	if len(describeRes.Descriptions) != 1 {
		t.Fatalf("Expected 1 user in Describe Result, got %d\n", len(describeRes.Descriptions))
	}
	description, ok := describeRes.Descriptions[users[0]]
	if !ok {
		t.Fatalf("Did not find expected user %s in results\n", users[0])
	}

	if description.Error.Code() != ErrResourceNotFound {
		t.Fatalf("Error should be ErrResourceNotFound instead it is %s", description.Error.Code())
	}

	// Call AlterUserScramCredentials for Upsert
	upsertions := []UserScramCredentialUpsertion{
		{
			User: "non-existent",
			ScramCredentialInfo: ScramCredentialInfo{
				Mechanism: ScramMechanismSHA256, Iterations: 10000},
			Password: []byte("password"),
			Salt:     []byte("salt"),
		}}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	alterRes, alterErr := ac.AlterUserScramCredentials(ctx, upsertions, nil)

	// Check Upsert result
	if alterErr != nil {
		t.Fatalf("Failed to Alter the User Scram Credentials: %s\n", alterErr)
	}
	if len(alterRes.Errors) != 1 {
		t.Fatalf("Expected 1 user in Alter Result, got %d\n", len(alterRes.Errors))
	}
	kErr, ok := alterRes.Errors[upsertions[0].User]
	if !ok {
		t.Fatalf("Did not find expected user %s in results\n", users[0])
	}
	if kErr.Code() != ErrNoError {
		t.Fatalf("Error code should be ErrNoError instead it is %d", kErr.Code())
	}

	// Call DescribeUserScramCredentials to verify upsert
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	describeRes, describeErr = ac.DescribeUserScramCredentials(ctx, users)

	// Check Describe result
	if describeErr != nil {
		t.Fatalf("Failed to Describe the User Scram Credentials: %s\n", describeErr)
	}
	description, ok = describeRes.Descriptions[users[0]]
	if !ok {
		t.Fatalf("Did not find expected user %s in results\n", users[0])
	}
	if description.Error.Code() != ErrNoError {
		t.Fatalf("Error code should be ErrNoError instead it is %s", description.Error.Code())
	}
	if description.ScramCredentialInfos[0].Iterations != 10000 {
		t.Fatalf("Iterations field doesn't match the upserted value. Expected 10000, got %d",
			description.ScramCredentialInfos[0].Iterations)
	}
	if description.ScramCredentialInfos[0].Mechanism != ScramMechanismSHA256 {
		t.Fatalf("Mechanism field doesn't match the upserted value. Expected %s, got %s",
			ScramMechanismSHA256, description.ScramCredentialInfos[0].Mechanism)
	}

	// Call AlterUserScramCredentials for Delete
	deletions := []UserScramCredentialDeletion{
		{User: "non-existent", Mechanism: ScramMechanismSHA256}}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	alterRes, alterErr = ac.AlterUserScramCredentials(ctx, nil, deletions)

	// Check Delete result
	if alterErr != nil {
		t.Fatalf("Failed to alter user scram credentials: %s\n", alterErr)
	}
	kErr, ok = alterRes.Errors[upsertions[0].User]
	if !ok {
		t.Fatalf("Did not find expected user %s in results\n", users[0])
	}
	if kErr.Code() != ErrNoError {
		t.Fatalf("Error code should be ErrNoError instead it is %d", kErr.Code())
	}

	// Call DescribeUserScramCredentials to verify delete
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	describeRes, describeErr = ac.DescribeUserScramCredentials(ctx, users)

	// Check Describe result
	if describeErr != nil {
		t.Fatalf("Failed to Describe the User Scram Credentials: %s\n", describeErr)
	}
	description, ok = describeRes.Descriptions[users[0]]
	if !ok {
		t.Fatalf("Did not find expected user %s in results\n", users[0])
	}

	if description.Error.Code() != ErrResourceNotFound {
		t.Fatalf("Error should be ErrResourceNotFound instead it is %s", description.Error.Code())
	}
}

// Tests ListOffsets API which describes
// the offset of a TopicPartition corresponding to the OffsetSpec provided.
func (its *IntegrationTestSuite) TestAdminClient_ListOffsets() {
	t := its.T()
	bootstrapServers := testconf.Brokers
	rand.Seed(time.Now().Unix())
	assert := its.Assert()

	// Create a new AdminClient.
	a := createAdminClient(t)
	defer a.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	topicPartitionOffsets := make(map[TopicPartition]OffsetSpec)
	Topic := fmt.Sprintf("%s-%d", testconf.TopicName, rand.Int())

	topics := []TopicSpecification{TopicSpecification{Topic: Topic, NumPartitions: 1, ReplicationFactor: 1}}
	createTopicResult, createTopicError := a.CreateTopics(ctx, topics)
	assert.Nil(createTopicError, "Create Topics should not fail.")
	assert.Equal(createTopicResult[0].Error.Code(), ErrNoError, "Create Topics Error Code should be ErrNoError.")

	p, err := NewProducer(&ConfigMap{"bootstrap.servers": bootstrapServers})
	assert.Nil(err, "Unable to create Producer.")
	defer p.Close()

	timestamp := time.Now()
	t1 := timestamp.Add(time.Second * 100)
	t2 := timestamp.Add(time.Second * 300)
	t3 := timestamp.Add(time.Second * 200)

	p.Produce(&Message{
		TopicPartition: TopicPartition{Topic: &Topic, Partition: 0},
		Value:          []byte("Message-1"),
		Timestamp:      t1,
	}, nil)

	p.Produce(&Message{
		TopicPartition: TopicPartition{Topic: &Topic, Partition: 0},
		Value:          []byte("Message-2"),
		Timestamp:      t2,
	}, nil)

	p.Produce(&Message{
		TopicPartition: TopicPartition{Topic: &Topic, Partition: 0},
		Value:          []byte("Message-3"),
		Timestamp:      t3,
	}, nil)

	p.Flush(5 * 1000)

	tp1 := TopicPartition{Topic: &Topic, Partition: 0}
	topicPartitionOffsets[tp1] = EarliestOffsetSpec
	var results ListOffsetsResult
	results, err = a.ListOffsets(ctx, topicPartitionOffsets, SetAdminIsolationLevel(IsolationLevelReadCommitted))
	assert.Nil(err, "ListOffsets should not fail.")

	for _, info := range results.ResultInfos {
		assert.Equal(ErrNoError, info.Error.Code(), "Error code should be ErrNoError.")
		assert.Equal(Offset(0), info.Offset, "Offset should be ErrNoError.")
	}

	topicPartitionOffsets[tp1] = LatestOffsetSpec
	results, err = a.ListOffsets(ctx, topicPartitionOffsets, SetAdminIsolationLevel(IsolationLevelReadCommitted))
	assert.Nil(err, "ListOffsets should not fail.")

	for _, info := range results.ResultInfos {
		assert.Equal(ErrNoError, info.Error.Code(), "Error code should be ErrNoError.")
		assert.Equal(Offset(3), info.Offset, "Offset should be 3.")
	}

	topicPartitionOffsets[tp1] = OffsetSpec(MaxTimestampOffsetSpec)
	results, err = a.ListOffsets(ctx, topicPartitionOffsets, SetAdminIsolationLevel(IsolationLevelReadCommitted))
	assert.Nil(err, "ListOffsets should not fail.")

	for _, info := range results.ResultInfos {
		assert.Equal(ErrNoError, info.Error.Code(), "Error code should be ErrNoError.")
		assert.Equal(Offset(1), info.Offset, "Offset should be 1.")
	}

	delTopics := []string{Topic}
	_, err = a.DeleteTopics(ctx, delTopics)
	assert.Nil(err, "DeleteTopics should not fail.")

}

// Test DeleteRecords API which deletes all the records before the specified offset
// in the particular partition of the specified topic.
func (its *IntegrationTestSuite) TestAdminClient_DeleteRecords() {
	t := its.T()
	bootstrapServers := testconf.Brokers
	assert := its.Assert()

	// Create a new AdminClient.
	a := createAdminClient(t)
	defer a.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a new topic to test the api and produce some messages to that topic
	topic := fmt.Sprintf("%s-%d", testconf.TopicName, rand.Int())

	topics := []TopicSpecification{
		{Topic: topic, NumPartitions: 3, ReplicationFactor: 1}}
	createTopicResult, createTopicError := a.CreateTopics(ctx, topics)
	assert.Nil(createTopicError, "Create topics should not fail.")
	assert.Equal(createTopicResult[0].Error.Code(), ErrNoError,
		"Create topics error code should be ErrNoError.")

	p, err := NewProducer(&ConfigMap{"bootstrap.servers": bootstrapServers})
	assert.Nil(err, "Unable to create Producer.")
	defer p.Close()

	numMessages := 3
	numPartitions := 3
	for i := 0; i < numMessages; i++ {
		for j := 0; j < numPartitions; j++ {
			p.Produce(&Message{
				TopicPartition: TopicPartition{Topic: &topic, Partition: int32(j)},
				Value:          []byte(fmt.Sprintf("Message-%d", i)),
			}, nil)
		}
	}

	p.Flush(5 * 1000)

	// Delete the records:
	// Partition 0: upto offset 2
	// Partition 1: upto the last offset
	// Partition 2: upto offset 4 (non-existent)
	// The result will contain the minimum offset available after deletion in
	// that Topic Partiton.
	delRecordsTopicPartitionOffsets := []TopicPartition{
		{Topic: &topic, Partition: int32(0), Offset: Offset(2)},
		{Topic: &topic, Partition: int32(1), Offset: OffsetEnd},
		{Topic: &topic, Partition: int32(2), Offset: Offset(4)},
	}
	deleteRes, err := a.DeleteRecords(ctx, delRecordsTopicPartitionOffsets)
	assert.Nil(err, "Delete Records should not fail")
	assert.Len(deleteRes.DeleteRecordsResults, 3,
		"Length of deleteRes.DeleteRecordsResults should be 3")

	assert.Nil(deleteRes.DeleteRecordsResults[0].TopicPartition.Error,
		"Error should not be set in deleteRes.DeleteRecordsResults[0]")
	assert.Nil(deleteRes.DeleteRecordsResults[1].TopicPartition.Error,
		"Error should not be set in deleteRes.DeleteRecordsResults[1]")
	assert.Error(deleteRes.DeleteRecordsResults[2].TopicPartition.Error,
		"Error should be set in deleteRes.DeleteRecordsResults[2]")
	assert.Nil(deleteRes.DeleteRecordsResults[2].DeletedRecords,
		"DeletedRecords should be nil within deleteRes.DeleteRecordsResults[2]")

	// Offsets after deletion reported by DeleteRecords, in order of topic
	// partitions.
	offsetAfterDeletion := []Offset{
		deleteRes.DeleteRecordsResults[0].DeletedRecords.LowWatermark,
		deleteRes.DeleteRecordsResults[1].DeletedRecords.LowWatermark,
	}

	// Expected minimum offsets at each partition (manually computed), in order
	// of topic partitions.
	offsetExpected := []Offset{Offset(2), Offset(3), Offset(0)}

	// Find the minimum offsets in the partitions of the topic via ListOffsets.
	// It should be equal to the offset we get after the deletion operation as
	// well as the expected minimum offsets.
	topicPartitionOffsets := map[TopicPartition]OffsetSpec{
		{Topic: &topic, Partition: 0}: EarliestOffsetSpec,
		{Topic: &topic, Partition: 1}: EarliestOffsetSpec,
		{Topic: &topic, Partition: 2}: EarliestOffsetSpec,
	}

	results, err := a.ListOffsets(ctx, topicPartitionOffsets)
	assert.Nil(err, "ListOffsets should not fail.")
	assert.Len(results.ResultInfos, 3,
		"Length of results.ResultInfos should be 3")

	for toppar, info := range results.ResultInfos {
		assert.Equal(info.Error.Code(), ErrNoError,
			"Error code should be ErrNoError.")

		// Compare the listed offset with the offset we expect the partition to be at.
		assert.Equal(offsetExpected[toppar.Partition], info.Offset,
			"Offset should be equal to %v on partition %d, got %v",
			offsetExpected[toppar.Partition], toppar.Partition, info.Offset)

		// If the DeleteRecords method for said partition succeeded, check if
		// it also matches the return from DeleteRecords.
		if int(toppar.Partition) < len(offsetAfterDeletion) {
			assert.Equal(info.Offset, offsetAfterDeletion[toppar.Partition],
				"Offset %v returned from DeleteRecords should be equal to the "+
					"ListOffsets result %v on partition %d.",
				offsetAfterDeletion[toppar.Partition], info.Offset, toppar.Partition)
		}
	}

	delTopics := []string{topic}
	_, err = a.DeleteTopics(ctx, delTopics)
	assert.Nil(err, "DeleteTopics should not fail.")
}

func TestIntegration(t *testing.T) {
	its := new(IntegrationTestSuite)
	testconfInit()
	if !testconfRead() {
		t.Skipf("testconf not provided or not usable\n")
		return
	}
	if testconf.DockerNeeded && !testconf.DockerExists {
		dockerCompose := "./testresources/docker-compose.yaml"
		if !testConsumerGroupProtocolClassic() {
			dockerCompose = "./testresources/docker-compose-kraft.yaml"
		}
		its.compose = compose.NewLocalDockerCompose([]string{dockerCompose}, "test-docker")
		execErr := its.compose.WithCommand([]string{"up", "-d"}).Invoke()
		if err := execErr.Error; err != nil {
			t.Fatalf("up -d command failed with the error message %s\n", err)
		}
		// It takes some time after the containers come up for them to be ready.
		time.Sleep(20 * time.Second)
	}
	suite.Run(t, its)
}
