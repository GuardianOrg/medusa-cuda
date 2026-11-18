package fuzzing

import (
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crytic/medusa-geth/common"
	"github.com/crytic/medusa/compilation/abiutils"
	"github.com/crytic/medusa/compilation/platforms"
	"github.com/crytic/medusa/fuzzing/bugfilter"
	"github.com/crytic/medusa/fuzzing/calls"
	"github.com/crytic/medusa/fuzzing/config"
	"github.com/crytic/medusa/fuzzing/contracts"
	"github.com/crytic/medusa/fuzzing/coverage"
	fuzzingutils "github.com/crytic/medusa/fuzzing/utils"

	"golang.org/x/exp/slices"
)

// AssertionTestCaseProvider is am AssertionTestCase provider which spawns test cases for every contract method and
// ensures that none of them result in a failed assertion (e.g. use of the solidity `assert(...)` statement, or special
// events indicating a failed assertion).
type AssertionTestCaseProvider struct {
	// fuzzer describes the Fuzzer which this provider is attached to.
	fuzzer *Fuzzer

	// testCases is a map of contract-method IDs to assertion test cases.GetContractMethodID
	testCases         map[contracts.ContractMethodID]*AssertionTestCase
	generalBugs       map[uint32]*AssertionTestCase // bug_id -> bool if encountered before
	falsePositiveBugs map[uint32]*AssertionTestCase // bug_id -> false positive bugs
	// testCasesLock is used for thread-synchronization when updating testCases
	testCasesLock sync.Mutex
}

// bugInfo holds PC and bug type information for false positive filtering
type bugInfo struct {
	pc      uint32
	bugType uint32
}

// attachAssertionTestCaseProvider attaches a new AssertionTestCaseProvider to the Fuzzer and returns it.
func attachAssertionTestCaseProvider(fuzzer *Fuzzer) *AssertionTestCaseProvider {
	// Create a test case provider
	t := &AssertionTestCaseProvider{
		fuzzer: fuzzer,
	}

	// Subscribe the provider to relevant events the fuzzer emits.
	fuzzer.Events.FuzzerStarting.Subscribe(t.onFuzzerStarting)
	fuzzer.Events.FuzzerStopping.Subscribe(t.onFuzzerStopping)
	fuzzer.Events.WorkerCreated.Subscribe(t.onWorkerCreated)

	// Add the provider's call sequence test function to the fuzzer.
	fuzzer.Hooks.CallSequenceTestFuncs = append(fuzzer.Hooks.CallSequenceTestFuncs, t.callSequencePostCallTest)
	return t
}

// checkAssertionFailures checks the results of the last call for assertion failures.
// Returns the method ID, a boolean indicating if an assertion test failed, or an error if one occurs.
func (t *AssertionTestCaseProvider) checkAssertionFailures(callSequence calls.CallSequence) (*contracts.ContractMethodID, bool, error) {
	// If we have an empty call sequence, we cannot have an assertion failure
	if len(callSequence) == 0 {
		return nil, false, nil
	}

	// Obtain the contract and method from the last call made in our sequence
	lastCall := callSequence[len(callSequence)-1]
	lastCallMethod, err := lastCall.Method()
	if err != nil {
		return nil, false, err
	}
	methodId := contracts.GetContractMethodID(lastCall.Contract, lastCallMethod)

	// Check if we encountered an enabled panic code.
	// Try to unpack our error and return data for a panic code and verify that that panic code should be treated as a failing case.
	// Solidity >0.8.0 introduced asserts failing as reverts but with special return data. But we indicate we also
	// want to be backwards compatible with older Solidity which simply hit an invalid opcode and did not actually
	// have a panic code.
	lastExecutionResult := lastCall.ChainReference.MessageResults().ExecutionResult
	panicCode := abiutils.GetSolidityPanicCode(lastExecutionResult.Err, lastExecutionResult.ReturnData, true)
	// fmt.Printf("CuEVM debug: Assertion check - Error: %v, ReturnData: %v, PanicCode: %v\n",
	// 	lastExecutionResult.Err,
	// 	lastExecutionResult.ReturnData,
	// 	panicCode)
	failure := false
	if panicCode != nil {
		failure = encounteredAssertionFailure(panicCode.Uint64(), t.fuzzer.config.Fuzzing.Testing.AssertionTesting.PanicCodeConfig)
	}

	return &methodId, failure, nil
}

// onFuzzerStarting is the event handler triggered when the Fuzzer is starting a fuzzing campaign. It creates test cases
// in a "not started" state for every method to test discovered in the contract definitions known to the Fuzzer.
func (t *AssertionTestCaseProvider) onFuzzerStarting(event FuzzerStartingEvent) error {
	// Reset our state
	t.testCases = make(map[contracts.ContractMethodID]*AssertionTestCase)
	t.generalBugs = make(map[uint32]*AssertionTestCase)
	t.falsePositiveBugs = make(map[uint32]*AssertionTestCase)
	// Create a test case for every test method.
	for _, contract := range t.fuzzer.ContractDefinitions() {
		// If we're not testing all contracts, verify the current contract is one we specified in our target contracts
		if !t.fuzzer.config.Fuzzing.Testing.TestAllContracts && !slices.Contains(t.fuzzer.config.Fuzzing.TargetContracts, contract.Name()) {
			continue
		}

		for _, method := range contract.AssertionTestMethods {
			// Create local variables to avoid pointer types in the loop being overridden.
			contract := contract
			method := method

			// Create our test case
			testCase := &AssertionTestCase{
				status:         TestCaseStatusNotStarted,
				targetContract: contract,
				targetMethod:   method,
				callSequence:   nil,
			}

			// Add to our test cases and register them with the fuzzer
			methodId := contracts.GetContractMethodID(contract, &method)
			t.testCases[methodId] = testCase
			t.fuzzer.RegisterTestCase(testCase)
		}
	}
	return nil
}

// onFuzzerStopping is the event handler triggered when the Fuzzer is stopping the fuzzing campaign and all workers
// have been destroyed. It clears state tracked for each FuzzerWorker and sets test cases in "running" states to
// "passed".
func (t *AssertionTestCaseProvider) onFuzzerStopping(event FuzzerStoppingEvent) error {
	// Loop through each test case and set any tests with a running status to a passed status.
	for _, testCase := range t.testCases {
		if testCase.status == TestCaseStatusRunning {
			testCase.status = TestCaseStatusPassed
		}
	}
	return nil
}

// onWorkerCreated is the event handler triggered when a FuzzerWorker is created by the Fuzzer. It ensures state tracked
// for that worker index is refreshed and subscribes to relevant worker events.
func (t *AssertionTestCaseProvider) onWorkerCreated(event FuzzerWorkerCreatedEvent) error {
	// Subscribe to relevant worker events.
	event.Worker.Events.ContractAdded.Subscribe(t.onWorkerDeployedContractAdded)
	return nil
}

// onWorkerDeployedContractAdded is the event handler triggered when a FuzzerWorker detects a new contract deployment
// on its underlying chain. It ensures any methods to test which the deployed contract contains are tracked by the
// provider for testing. Any test cases previously made for these methods which are in a "not started" state are put
// into a "running" state, as they are now potentially reachable for testing.
func (t *AssertionTestCaseProvider) onWorkerDeployedContractAdded(event FuzzerWorkerContractAddedEvent) error {
	// If we don't have a contract definition, we can't run tests against the contract.
	if event.ContractDefinition == nil {
		return nil
	}

	// Loop through all methods and find ones for which we have tests
	for _, method := range event.ContractDefinition.CompiledContract().Abi.Methods {
		// Obtain an identifier for this pair
		methodId := contracts.GetContractMethodID(event.ContractDefinition, &method)

		// If we have a test case targeting this contract/method that has not failed, track this deployed method in
		// our map for this worker. If we have any tests in a not-started state, we can signal a running state now.
		t.testCasesLock.Lock()
		testCase, testCaseExists := t.testCases[methodId]
		t.testCasesLock.Unlock()
		if testCaseExists && testCase.Status() == TestCaseStatusNotStarted {
			testCase.status = TestCaseStatusRunning
		}
	}
	return nil
}

// callSequencePostCallTest provides is a CallSequenceTestFunc that performs post-call testing logic for the attached Fuzzer
// and any underlying FuzzerWorker. It is called after every call made in a call sequence. It checks whether invariants
// in methods to test are upheld after each call the Fuzzer makes when testing a call sequence.
func (t *AssertionTestCaseProvider) callSequencePostCallTest(worker *FuzzerWorker, callSequence calls.CallSequence) ([]ShrinkCallSequenceRequest, error) {
	// fmt.Println("CuEVM Debug: callSequencePostCallTest")
	// Create a list of shrink call sequence verifiers, which we populate for each failed test we want a call sequence
	// shrunk for.
	shrinkRequests := make([]ShrinkCallSequenceRequest, 0)

	// Obtain the method ID for the last call and check if it encountered assertion failures.
	methodId, testFailed, err := t.checkAssertionFailures(callSequence)
	if err != nil {
		return nil, err
	}

	// Obtain the test case for this method we're targeting for assertion testing.
	t.testCasesLock.Lock()
	testCase, testCaseExists := t.testCases[*methodId]
	t.testCasesLock.Unlock()
	// fmt.Println("CuEVM Debug: testCase", testCase, "testCaseExists", testCaseExists)
	// Verify a test case exists for this method called (if we're not assertion testing this method, stop)
	if !testCaseExists {
		return shrinkRequests, nil
	}

	// If the test case already failed, skip it
	if testCase.Status() == TestCaseStatusFailed {
		return shrinkRequests, nil
	}

	// If we failed a test, we update our state immediately. We provide a shrink verifier which will update
	// the call sequence for each shrunken sequence provided that fails the test.
	if testFailed {
		// Create a request to shrink this call sequence.
		shrinkRequest := ShrinkCallSequenceRequest{
			TestName:             testCase.Name(),
			CallSequenceToShrink: callSequence,
			VerifierFunction: func(worker *FuzzerWorker, shrunkenCallSequence calls.CallSequence) (bool, error) {
				// Obtain the method ID for the last call and check if it encountered assertion failures.
				shrunkSeqMethodId, shrunkSeqTestFailed, err := t.checkAssertionFailures(shrunkenCallSequence)
				if err != nil {
					return false, err
				}

				// If we encountered assertion failures on the same method, this shrunk sequence is satisfactory.
				return shrunkSeqTestFailed && *methodId == *shrunkSeqMethodId, nil
			},
			FinishedCallback: func(worker *FuzzerWorker, shrunkenCallSequence calls.CallSequence, verbosity config.VerbosityLevel) error {
				// When we're finished shrinking, attach an execution trace to the last call. If verboseTracing is true, attach to all calls.
				if len(shrunkenCallSequence) > 0 {
					_, err = calls.ExecuteCallSequenceWithExecutionTracer(worker.chain, worker.fuzzer.contractDefinitions, shrunkenCallSequence, verbosity)
					if err != nil {
						return err
					}
				}

				// Update our test state and report it finalized.
				testCase.status = TestCaseStatusFailed
				testCase.callSequence = &shrunkenCallSequence
				worker.workerMetrics().failedSequences.Add(worker.workerMetrics().failedSequences, big.NewInt(1))
				worker.Fuzzer().ReportTestCaseFinished(testCase)
				return nil
			},
			RecordResultInCorpus: true,
		}

		// Add our shrink request to our list.
		shrinkRequests = append(shrinkRequests, shrinkRequest)
	}

	return shrinkRequests, nil
}

// GPUPostCallTest provides is a CallSequenceTestFunc that performs post-call testing logic for the attached Fuzzer
// and any underlying FuzzerWorker. It is called after every call made in a call sequence. It checks whether invariants
// in methods to test are upheld after each call the Fuzzer makes when testing a call sequence.
func (t *AssertionTestCaseProvider) GPUPostCallTest(workers []*FuzzerWorker, gpuResult *coverage.GPUExecutionResult, markerOffsets []int32, bigIntWeightValue *big.Int) (bool, error) {

	skipSequenceSize := t.fuzzer.skipSequenceSize
	txBatchSizeCPU := t.fuzzer.sequencesPerCPUWorker * t.fuzzer.numCPUWorkers
	txBatchSizeGPU := t.fuzzer.sequencesPerCPUWorker * t.fuzzer.numCPUWorkers * t.fuzzer.skipSequenceSize
	total_bugs_encountered := 0
	for batchIdx := 0; batchIdx < len(gpuResult.NewBugThreadIdx); batchIdx++ {
		total_bugs_encountered += len(gpuResult.NewBugThreadIdx[batchIdx])
		for idx := 0; idx < len(gpuResult.NewBugThreadIdx[batchIdx]); idx++ {
			// translate from gpu idx to cpu idx (no skip sequence)
			rawIdx := int(gpuResult.NewBugThreadIdx[batchIdx][idx]) / skipSequenceSize
			rawPC := gpuResult.NewBugPCs[batchIdx][idx]
			bugType := gpuResult.NewBugTypes[batchIdx][idx]
			bugContractId := gpuResult.NewBugContractIds[batchIdx][idx]
			workerIdx := rawIdx / t.fuzzer.sequencesPerCPUWorker
			sequenceIdx := rawIdx % t.fuzzer.sequencesPerCPUWorker
			elementIdx := batchIdx
			fullSequence := make(calls.CallSequence, elementIdx+1)
			for i := 0; i <= elementIdx; i++ {
				fullSequence[i], _ = workers[workerIdx].callSequenceElements[sequenceIdx][i].Clone()
				// fmt.Println("CuEVM Debug: fullSequence[i] original", hex.EncodeToString(fullSequence[i].Call.Data), "abi values", fullSequence[i].Call.DataAbiValues)
				// Warning i is element index in a squence
				// Need to reply i when reconstructing the sequence
				markerOffsetIdx := (rawIdx + i*txBatchSizeCPU)
				methodSig := fullSequence[i].Call.DataAbiValues.Method.Sig
				// fmt.Println("CuEVM Debug: markerOffsetIdx", markerOffsetIdx, "methodSig", methodSig, "rawIdx", rawIdx, "i", i)
				var dataMarkers []calls.DataMarker
				if markerOffsets[markerOffsetIdx] < 0 {
					dataMarkers = t.fuzzer.staticABIMarkers[t.fuzzer.staticABIMarkerIndexMap[methodSig]]
				} else {
					// fmt.Println("CuEVM Debug: markerOffsets[markerOffsetIdx] (sequenceIdx/f.skipSequenceSize)*f.skipSequenceSize", markerOffsets[markerOffsetIdx], "sequenceIdx", sequenceIdx, "i", i)
					dataMarkers = workers[workerIdx].callSequenceElements[sequenceIdx][i].Call.DataMarkers
				}
				// fmt.Println("CuEVM Debug: dataMarkers", dataMarkers)
				// fmt.Println("CuEVM Debug: fullSequence[i].Call.Data", hex.EncodeToString(fullSequence[i].Call.Data))
				mutatedData, mutatedBlockNumber, mutatedBlockTimestamp, mutatedSenderIndex, mutatedValue := fuzzingutils.RestoreMutation(fullSequence[i].Call.Data, dataMarkers, int(gpuResult.NewBugThreadIdx[batchIdx][idx]), i, fuzzingutils.FuzzerConfig{
					StartSeed:              t.fuzzer.currentRandomSeed,
					BatchSize:              uint32(txBatchSizeGPU),
					NumInstancesPerDevice:  t.fuzzer.numInstancesPerDevice,
					AddressConstants:       t.fuzzer.addressConstants,
					IntegerConstants:       t.fuzzer.integerConstants,
					BlockNumberDelayMax:    60480 * 2, // hardcode for now
					BlockTimestampDelayMax: 604800 * 4,
					SenderCount:            uint32(len(t.fuzzer.senders)),
					IsReentrancySender:     fullSequence[i].Call.From == common.HexToAddress(REENTRANCY_ATTACKER_ADDRESS),
					IsRandomSender:         fullSequence[i].Call.From == common.HexToAddress(RANDOM_ATTACKER_ADDRESS),
				})
				// fmt.Println("CuEVM Debug: mutatedData", hex.EncodeToString(mutatedData))

				// Apply mutated block values if they were changed (non-zero)
				if i == 0 {
					fullSequence[i].BlockNumberDelay = max(1, workers[workerIdx].callSequenceElements[sequenceIdx][0].BlockNumberDelay) + uint64(max(1, int64(mutatedBlockNumber))) - 1          // first block is 1
					fullSequence[i].BlockTimestampDelay = max(1, workers[workerIdx].callSequenceElements[sequenceIdx][0].BlockTimestampDelay) + uint64(max(1, int64(mutatedBlockTimestamp))) - 1 // first block is 1
				} else {
					fullSequence[i].BlockNumberDelay = uint64(max(1, int64(mutatedBlockNumber)))
					fullSequence[i].BlockTimestampDelay = uint64(max(1, int64(mutatedBlockTimestamp)))
				}

				if mutatedSenderIndex >= 0 {
					fullSequence[i].Call.From = t.fuzzer.senders[mutatedSenderIndex]
				}

				// Apply mutated value if it was changed (non-zero)

				fullSequence[i].Call.Value = mutatedValue
				var inputValues []any
				var err error
				if len(mutatedData) >= 4 {
					if fullSequence[i].Call.DataAbiValues.Method.Sig != "CuEVM::fallback()" {
						inputData := mutatedData[4:] // skip the method ID
						inputValues, err = fullSequence[i].Call.DataAbiValues.Method.Inputs.Unpack(inputData)
					} else {
						inputValues, err = fullSequence[i].Call.DataAbiValues.Method.Inputs.Unpack(mutatedData)
					}

					if err != nil {
						fmt.Println("\n\nCuEVM Debug: inputValues unpack error\n\n", err)
						// skip unpack if err occurs
						err = nil
					}
				} else {
					fmt.Println("CuEVM Debug: inputValues empty")
					inputValues = []any{}
				}

				// fmt.Println("CuEVM debug original data ", hex.EncodeToString(fullSequence[i].Call.Data))
				fullSequence[i].Call.DataAbiValues.InputValues = inputValues
				fullSequence[i].Call.Data = mutatedData
				// fmt.Println("CuEVM debug mutated data ", hex.EncodeToString(mutatedData))
				// fmt.Println("CuEVM debug new inputValues", inputValues)
				// fmt.Println("CuEVM debug mutated sender index", mutatedSenderIndex)
				// fmt.Println("CuEVM debug mutated value", mutatedValue)
				// fmt.Println("CuEVM debug mutated block number", mutatedBlockNumber)
				// fmt.Println("CuEVM debug mutated block timestamp", mutatedBlockTimestamp)

				// fmt.Println("Call element", fullSequence[i])
			}
			// workers[workerIdx].fuzzer.corpus.AddCallSequence(fullSequence, bigIntWeightValue)
			lastCall := fullSequence[len(fullSequence)-1]
			lastCallMethod, err := lastCall.Method()
			if err != nil {
				continue
			}
			// methodId := contracts.GetContractMethodID(lastCall.Contract, lastCallMethod)
			/* Jul : temporarily disable native assertion bug type. To be used with general bugs
			if bugType == CuEVM_ASSERTION_BUG_TYPE {
				testFailed := encounteredAssertionFailure(1, t.fuzzer.config.Fuzzing.Testing.AssertionTesting.PanicCodeConfig)

				t.testCasesLock.Lock()
				testCase, testCaseExists := t.testCases[methodId]
				t.testCasesLock.Unlock()
				if err != nil {
					continue
				}
				if !testCaseExists {
					continue
				}

				if testCase.Status() == TestCaseStatusFailed {
					continue
				}
				// if _, exists := bugPCsProcessed[rawPC]; exists {
				// 	continue
				// }

				if testFailed {

					// Create a request to shrink this call sequence.
					shrinkRequest := ShrinkCallSequenceRequest{
						TestName:             testCase.Name(),
						CallSequenceToShrink: fullSequence,
						VerifierFunction: func(shrinkVerifierWorker *FuzzerWorker, shrunkenCallSequence calls.CallSequence) (bool, error) {
							shrunkSeqMethodId, shrunkSeqTestFailed, errVerify := t.checkAssertionFailures(shrunkenCallSequence)
							if errVerify != nil {
								return false, errVerify
							}
							return shrunkSeqTestFailed && methodId == *shrunkSeqMethodId, nil
						},
						FinishedCallback: func(finishedCallbackWorker *FuzzerWorker, shrunkenCallSequence calls.CallSequence, verbosity config.VerbosityLevel) error {
							if len(shrunkenCallSequence) > 0 {
								_, errCb := calls.ExecuteCallSequenceWithExecutionTracer(finishedCallbackWorker.chain, finishedCallbackWorker.fuzzer.contractDefinitions, shrunkenCallSequence, verbosity)
								if errCb != nil {
									return errCb
								}
							}
							testCase.status = TestCaseStatusFailed
							testCase.callSequence = &shrunkenCallSequence
							finishedCallbackWorker.workerMetrics().failedSequences.Add(finishedCallbackWorker.workerMetrics().failedSequences, big.NewInt(1))
							finishedCallbackWorker.Fuzzer().ReportTestCaseFinished(testCase)
							return nil
						},
						RecordResultInCorpus: true,
					}
					// newShrinkRequests[workerIdx] = append(newShrinkRequests[workerIdx], shrinkRequest)
					workers[0].pendingShrinkRequests = append(workers[0].pendingShrinkRequests, shrinkRequest)

				}

			}
			*/
			// general bugs including assertion failure, to be exported to json later
			{
				// An assertion bug is only a test failure when the configuration says so.
				if (bugType == CuEVM_ASSERTION_BUG_TYPE || bugType == CuEVM_INVALID_OPCODE) &&
					!t.fuzzer.config.Fuzzing.Testing.AssertionTesting.PanicCodeConfig.FailOnAssertion {
					continue
				}
				bug_id := rawPC<<16 | bugType<<8 | (bugContractId & 0xFF)
				// fmt.Println("Bug Raw PC", rawPC, "Bug Type", bugType, "Bug Contract ID", bugContractId, "fuzzer target contract id", t.fuzzer.targetContractId)
				if _, exists := t.generalBugs[bug_id]; exists {
					continue
				}
				// Filter false positive for arbitrary call: skip if method has no dynamic bytes input
				if bugType == CuEVM_ARBITRARY_CALL {
					if hasBytesInput, ok := workers[workerIdx].sigHasBytesCache[lastCallMethod.Sig]; !ok || !hasBytesInput {
						continue
					}
				}
				if bugType == CuEVM_LEAKING_ETHER || bugType == CuEVM_REENTRANCY {

					has_deployer_action := false
					for i := 0; i < len(fullSequence); i++ {
						method, err := fullSequence[i].Method()
						if err != nil {
							fmt.Println("CuEVM Debug: method error", err)
							continue
						}
						methodSigStr := strings.ToLower(method.Sig)
						if strings.HasPrefix(methodSigStr, "initialize") {
							has_deployer_action = true
							break
						}
						if fullSequence[i].Call.From == t.fuzzer.DeployerAddress() {
							if strings.HasPrefix(methodSigStr, "change") || strings.HasPrefix(methodSigStr, "set") || strings.HasPrefix(methodSigStr, "init") {
								has_deployer_action = true
								// fmt.Println("CuEVM Debug: has_deployer_action", has_deployer_action)
								break
							}
						}
					}
					if has_deployer_action {
						// treat as false positive
						continue
					}
				}
				if bugContractId != t.fuzzer.targetContractId {
					if bugType == CuEVM_INTEGER_ADD || bugType == CuEVM_INTEGER_SUB || bugType == CuEVM_INTEGER_MUL {
						continue
					}
				}
				// RegisterTestCase registers a new TestCase with the Fuzzer.
				testCase := &AssertionTestCase{
					status:          TestCaseStatusFailed,
					targetContract:  lastCall.Contract,
					targetMethod:    *lastCallMethod,
					bugType:         bugType,
					bugPC:           rawPC,
					bugContractName: t.fuzzer.targetContractName,
					bugContractId:   bugContractId,
					callSequence:    &fullSequence,
					bugTime:         time.Since(t.fuzzer.fuzzStartTime).Seconds(), // seconds
				}

				// Add all bugs to general bugs first, false positive filtering happens later
				t.fuzzer.RegisterTestCase(testCase)
				t.generalBugs[bug_id] = testCase
				select {
				case t.fuzzer.shrinkWorker.addSequenceCorpusChan <- AddSequenceCorpusRequest{
					Sequence: fullSequence,
					Weight:   bigIntWeightValue,
				}:
					// Successfully enqueued
				default:
					fmt.Println("addSequenceCorpusChan full, adding sequence directly to corpus")
					_ = t.fuzzer.corpus.AddCallSequence(fullSequence, bigIntWeightValue)
				}

			}
			// bugPCsProcessed[rawPC] = true
		}

	}

	// CuEVM: disable shrink requests for debugging June 19
	// for workerIdx := 0; workerIdx < len(workers); workerIdx++ {
	// 	worker := workers[workerIdx]
	// 	if len(newShrinkRequests[workerIdx]) > 0 {
	// 		worker.pendingShrinkRequests = append(worker.pendingShrinkRequests, newShrinkRequests[workerIdx]...)

	// 	}
	// 	// fmt.Println("CuEVM Debug: workerIdx", workerIdx, "pendingShrinkRequests", len(worker.pendingShrinkRequests))
	// }
	// fmt.Println("CuEVM Debug: total_bugs_encountered", total_bugs_encountered)
	return total_bugs_encountered > 0, nil
}

// pythonFalsePositivePCs gets false positive PCs for a contract's arithmetic bugs with solidityutils/filter_fp.py,
// which compiles the contract again with solc to map the PCs to source code. It is only used for an Etherscan target
// whose source files medusa can't read, since PCs from another compiler's bytecode (such as Solar's) map to the wrong
// code in solc's.
func (t *AssertionTestCaseProvider) pythonFalsePositivePCs(contractName string, bugs []bugInfo) map[uint32]bool {
	fpPCs := make(map[uint32]bool)

	if len(bugs) == 0 {
		return fpPCs
	}

	// Helper to mark all bugs as false positives (conservative fallback)
	allBugsAsFP := func() map[uint32]bool {
		result := make(map[uint32]bool)
		for _, bug := range bugs {
			result[bug.pc] = true
		}
		return result
	}

	// Retrieve etherscan flag and target from platform config
	var etherscanFlag bool
	var target string
	if platformConfig, err := t.fuzzer.config.Compilation.GetPlatformConfig(); err == nil {
		if cryticConfig, ok := platformConfig.(*platforms.CryticCompilationConfig); ok {
			etherscanFlag = cryticConfig.EtherscanJsonFile
			target = cryticConfig.Target

		}
	}

	// Determine source path based on etherscan flag
	var sourcePath string
	if etherscanFlag {
		sourcePath = target
	} else {
		// Find source path for the contract
		for _, contract := range t.fuzzer.ContractDefinitions() {

			if contract.Name() == contractName {
				sourcePath = contract.SourcePath()
				break
			}
		}
	}
	// fmt.Println("CuEVM Debug: sourcePath", sourcePath)
	if sourcePath == "" {
		fmt.Println("CuEVM Debug: no source path found", contractName)
		return allBugsAsFP()
	}

	// Find script path
	executablePath, _ := os.Executable()

	executableDir := filepath.Dir(executablePath)
	scriptPath := filepath.Join(executableDir, "solidityutils", "filter_fp.py")

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		scriptPath = "solidityutils/filter_fp.py"
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			fmt.Println("CuEVM Debug: script not found", scriptPath)
			return fpPCs
		}
	}

	// Prepare command arguments
	platformConfig, _ := t.fuzzer.config.Compilation.GetPlatformConfig()
	cryticConfig, ok := platformConfig.(*platforms.CryticCompilationConfig)
	if ok {
		fmt.Println("Solc Version:", cryticConfig.SolcVersion)
	} else {
		fmt.Println("platformConfig is not of type CryticCompilationConfig")
	}
	args := []string{scriptPath, contractName, sourcePath, cryticConfig.SolcVersion}
	for _, bug := range bugs {
		args = append(args, fmt.Sprintf("%d:%d", bug.pc, bug.bugType))
	}

	// Execute Python script
	cmd := exec.Command("python3", args...)
	fmt.Println("CuEVM Debug: cmd", cmd)
	output, err := cmd.Output()
	fmt.Println("CuEVM Debug: output", string(output))
	if err != nil {
		fmt.Println("CuEVM Debug: error executing script", err)
		return allBugsAsFP()
	}

	// Parse output - space-separated list of false positive PCs (get only last line of output)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	outputStr := ""
	if len(lines) > 0 {
		outputStr = strings.TrimSpace(lines[len(lines)-1])
	}
	// fmt.Println("CuEVM Debug: outputStr", outputStr)
	if outputStr == "" {
		// fmt.Println("CuEVM Debug: no false positives found")
		return fpPCs // No false positives
	}

	fpPCStrs := strings.Fields(outputStr)
	for _, pcStr := range fpPCStrs {
		var parsedPC uint32
		if _, err := fmt.Sscanf(pcStr, "%d", &parsedPC); err == nil {
			fpPCs[parsedPC] = true
		}
	}

	return fpPCs
}

func (t *AssertionTestCaseProvider) getAllBugReported() []*AssertionTestCase {
	// First, process false positives by batching arithmetic bugs by contract
	t.processFalsePositives()

	validBugs := make([]*AssertionTestCase, 0, len(t.generalBugs))
	fpBugs := make([]*AssertionTestCase, 0, len(t.falsePositiveBugs))

	// Separate valid bugs and false positives
	for _, v := range t.generalBugs {
		validBugs = append(validBugs, v)
	}
	t.addBugSources(validBugs)

	for _, v := range t.falsePositiveBugs {
		fpBugs = append(fpBugs, v)
	}

	fmt.Printf("CuEVM Debug: Found %d valid bugs and %d false positives:\n", len(validBugs), len(fpBugs))
	for i, bug := range validBugs {
		fmt.Printf("  CuEVM_BUG_REPORT %d: Contract=%s, Method=%s, Type=%d, PC=%d, Time=%f\n",
			i, bug.bugContractName, bug.targetMethod.Name, bug.bugType, bug.bugPC, bug.bugTime)
		if bug.bugSource != nil && !bug.bugSource.IsGenerated() {
			fmt.Printf("    at %s:%d: %s\n", bug.bugSource.SourcePath, bug.bugSource.StartLine,
				strings.TrimSpace(bug.bugSource.Fragment))
		}
	}

	if len(fpBugs) > 0 {
		fmt.Printf("CuEVM Debug: False positives filtered:\n")
		for i, bug := range fpBugs {
			fmt.Printf("  CuEVM_FP %d: Contract=%s, Method=%s, Type=%d, PC=%d, Time=%f\n",
				i, bug.bugContractName, bug.targetMethod.Name, bug.bugType, bug.bugPC, bug.bugTime)
		}
	}

	return validBugs
}

// processFalsePositives filters false positives out of the arithmetic bugs. Each bug's PC is mapped to source code
// with the source map of the bytecode medusa compiled and CuEVM ran, so solc and Solar builds are both handled (see
// bugfilter.CheckArithmeticBug). False positives move from generalBugs to falsePositiveBugs.
func (t *AssertionTestCaseProvider) processFalsePositives() {
	// Group arithmetic bugs by contract
	contractBugs := make(map[string][]uint32) // contract -> bug ids
	for bug_id, bug := range t.generalBugs {
		if _, ok := arithmeticOp(bug.bugType); ok {
			contractBugs[bug.bugContractName] = append(contractBugs[bug.bugContractName], bug_id)
		}
	}

	for contractName, bugIds := range contractBugs {
		contract := t.contractDefinition(contractName)
		if contract == nil {
			fmt.Println("CuEVM Debug: no contract definition found", contractName)
			for _, bug_id := range bugIds {
				t.markFalsePositive(bug_id)
			}
			continue
		}

		// An Etherscan target's sources may not be on disk. Fall back to compiling it again with solc.
		if t.isEtherscanTarget() && len(contract.Compilation().SourceCode[contract.SourcePath()]) == 0 {
			bugs := make([]bugInfo, 0, len(bugIds))
			for _, bug_id := range bugIds {
				bugs = append(bugs, bugInfo{t.generalBugs[bug_id].bugPC, t.generalBugs[bug_id].bugType})
			}
			fpPCs := t.pythonFalsePositivePCs(contractName, bugs)
			for _, bug_id := range bugIds {
				if fpPCs[t.generalBugs[bug_id].bugPC] {
					t.markFalsePositive(bug_id)
				}
			}
			continue
		}

		for _, bug_id := range bugIds {
			bug := t.generalBugs[bug_id]
			op, _ := arithmeticOp(bug.bugType)
			verdict := bugfilter.CheckArithmeticBug(contract.Compilation(), contract.CompiledContract(),
				uint64(bug.bugPC), op)
			bug.bugSource = verdict.Source
			if verdict.FalsePositive {
				fmt.Printf("CuEVM Debug: Filtered FP - Contract=%s, PC=%d, Reason=%s\n", contractName, bug.bugPC,
					verdict.Reason)
				t.markFalsePositive(bug_id)
			} else {
				fmt.Printf("CuEVM Debug: Bug kept - Contract=%s, PC=%d, %s\n", contractName, bug.bugPC, verdict.Reason)
			}
		}
	}
}

// markFalsePositive moves a bug from generalBugs to falsePositiveBugs.
func (t *AssertionTestCaseProvider) markFalsePositive(bug_id uint32) {
	if bug, exists := t.generalBugs[bug_id]; exists {
		delete(t.generalBugs, bug_id)
		t.falsePositiveBugs[bug_id] = bug
	}
}

// contractDefinition returns the definition of the contract with the given name, or nil if there is none.
func (t *AssertionTestCaseProvider) contractDefinition(contractName string) *contracts.Contract {
	for _, contract := range t.fuzzer.ContractDefinitions() {
		if contract.Name() == contractName {
			return contract
		}
	}
	return nil
}

// isEtherscanTarget reports whether the fuzzer compiled an Etherscan JSON file.
func (t *AssertionTestCaseProvider) isEtherscanTarget() bool {
	platformConfig, err := t.fuzzer.config.Compilation.GetPlatformConfig()
	if err != nil {
		return false
	}
	cryticConfig, ok := platformConfig.(*platforms.CryticCompilationConfig)
	return ok && cryticConfig.EtherscanJsonFile
}

// addBugSources maps the PC of each bug in the target contract to source code, for bugs.json and the logs. Bugs in
// other contracts keep no source, since their PCs are in another contract's bytecode.
func (t *AssertionTestCaseProvider) addBugSources(bugs []*AssertionTestCase) {
	contract := t.contractDefinition(t.fuzzer.targetContractName)
	if contract == nil {
		return
	}
	for _, bug := range bugs {
		if bug.bugSource != nil || bug.bugContractId != t.fuzzer.targetContractId || bug.bugPC == 0 {
			continue
		}
		if source, err := contract.Compilation().SourceRangeAtPC(contract.CompiledContract(), uint64(bug.bugPC), true); err == nil {
			bug.bugSource = source
		}
	}
}

// arithmeticOp returns the operation of an arithmetic bug type, or false for a bug type that is not arithmetic.
func arithmeticOp(bugType uint32) (bugfilter.ArithmeticOp, bool) {
	switch bugType {
	case CuEVM_INTEGER_ADD:
		return bugfilter.Add, true
	case CuEVM_INTEGER_SUB:
		return bugfilter.Sub, true
	case CuEVM_INTEGER_MUL:
		return bugfilter.Mul, true
	case CuEVM_INTEGER_BUG:
		return bugfilter.AnyArithmetic, true
	}
	return bugfilter.AnyArithmetic, false
}

// encounteredAssertionFailure takes in a panic code and a config.AssertionModesConfig and will determine whether the
// panic code that was hit should be treated as a failing case - which will be determined by whether that panic
// code was enabled in the config. Note that the panic codes are defined in the abiutils package and that this function
// panic if it is provided a panic code that is not defined in the abiutils package.
// TODO: This is a terrible design and a future PR should be made to maintain assertion and panic logic correctly
func encounteredAssertionFailure(panicCode uint64, conf config.PanicCodeConfig) bool {
	// Switch on panic code
	switch panicCode {
	case abiutils.PanicCodeCompilerInserted:
		return conf.FailOnCompilerInsertedPanic
	case abiutils.PanicCodeAssertFailed:
		return conf.FailOnAssertion
	case abiutils.PanicCodeArithmeticUnderOverflow:
		return conf.FailOnArithmeticUnderflow
	case abiutils.PanicCodeDivideByZero:
		return conf.FailOnDivideByZero
	case abiutils.PanicCodeEnumTypeConversionOutOfBounds:
		return conf.FailOnEnumTypeConversionOutOfBounds
	case abiutils.PanicCodeIncorrectStorageAccess:
		return conf.FailOnIncorrectStorageAccess
	case abiutils.PanicCodePopEmptyArray:
		return conf.FailOnPopEmptyArray
	case abiutils.PanicCodeOutOfBoundsArrayAccess:
		return conf.FailOnOutOfBoundsArrayAccess
	case abiutils.PanicCodeAllocateTooMuchMemory:
		return conf.FailOnAllocateTooMuchMemory
	case abiutils.PanicCodeCallUninitializedVariable:
		return conf.FailOnCallUninitializedVariable
	default:
		// If we encounter an unknown panic code, we ignore it
		return false
	}
}
