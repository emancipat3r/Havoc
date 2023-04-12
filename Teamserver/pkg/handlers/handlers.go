package handlers

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/bits"

	"Havoc/pkg/agent"
	"Havoc/pkg/common/packer"
	"Havoc/pkg/common/parser"
	"Havoc/pkg/logger"
)

// parseAgentRequest
// parses the agent request and handles the given data.
// return 2 types.
// Response is the data/bytes once this function finished parsing the request.
// Success is if the function was successful while parsing the agent request.
//
//		Response byte.Buffer
//	 Success	 bool
func parseAgentRequest(Teamserver agent.TeamServer, Body []byte) (bytes.Buffer, bool) {

	var (
		Header   agent.Header
		Response bytes.Buffer
		err      error
	)

	Header, err = agent.AgentParseHeader(Body)
	if err != nil {
		logger.Debug("[Error] Header: " + err.Error())
		return Response, false
	}

	if Header.Data.Length() < 4 {
		return Response, false
	}

	/* handle this demon connection if the magic value matches */
	if Header.MagicValue == agent.DEMON_MAGIC_VALUE {
		return handleDemonAgent(Teamserver, Header)
	}

	/* If it's not a Demon request then try to see if it's a 3rd party agent. */
	return handleServiceAgent(Teamserver, Header)
}

func handleDemonAgent(Teamserver agent.TeamServer, Header agent.Header) (bytes.Buffer, bool) {

	var (
		Agent    *agent.Agent
		Response bytes.Buffer
		Command  = 0
		Packer  *packer.Packer
		Build   []byte
		err      error
	)

	/* check if the agent exists. */
	if Teamserver.AgentExist(Header.AgentID) {

		/* get our agent instance based on the agent id */
		Agent = Teamserver.AgentInstance(Header.AgentID)
		Command = Header.Data.ParseInt32()

		/* check if we received a response and if we tasked once.
		 * if not then this is weird... really weird so better reject it. */
		if Command != agent.COMMAND_GET_JOB && Agent.TaskedOnce {
			Agent.TaskDispatch(Command, Header.Data, Teamserver)
		}

		/* check if this is a 'reconnect' request */
		if Command == agent.DEMON_INIT {
			Packer = packer.NewPacker(Agent.Encryption.AESKey, Agent.Encryption.AESIv)
			Packer.AddUInt32(uint32(Header.AgentID))

			Build = Packer.Build()

			_, err = Response.Write(Build)
			if err != nil {
				logger.Error(err)
				return Response, false
			}
			logger.Debug(fmt.Sprintf("reconnected %x", Build))
			return Response, true
		}

		if Command == agent.COMMAND_GET_JOB {

			if !Agent.TaskedOnce {
				Agent.TaskedOnce = true
			}

			Agent.UpdateLastCallback(Teamserver)

			/* if there is no job then just reply with a COMMAND_NOJOB */
			if len(Agent.JobQueue) == 0 {
				var NoJob = []agent.Job{{
					Command: agent.COMMAND_NOJOB,
					Data:    []interface{}{},
				}}

				var Payload = agent.BuildPayloadMessage(NoJob, Agent.Encryption.AESKey, Agent.Encryption.AESIv)

				_, err = Response.Write(Payload)
				if err != nil {
					logger.Error("Couldn't write to HTTP connection: " + err.Error())
					return Response, false
				}

				/* if there is a job then send the Task Queue */
			} else {
				var (
					job     = Agent.GetQueuedJobs()
					payload = agent.BuildPayloadMessage(job, Agent.Encryption.AESKey, Agent.Encryption.AESIv)
				)

				_, err = Response.Write(payload)
				if err != nil {
					logger.Error("Couldn't write to HTTP connection: " + err.Error())
					return Response, false
				}

				/* show bytes for pivot */
				var CallbackSizes = make(map[int64][]byte)
				for j := range job {

					if len(job[j].Data) >= 1 {
						CallbackSizes[int64(Header.AgentID)] = append(CallbackSizes[int64(Header.AgentID)], payload...)
						continue
					}

					switch job[j].Command {

					case agent.COMMAND_PIVOT:

						if job[j].Data[0] == agent.AGENT_PIVOT_SMB_COMMAND {

							var (
								TaskBuffer    = job[j].Data[2].([]byte)
								PivotAgentID  = int(job[j].Data[1].(int64))
								PivotInstance *agent.Agent
							)

							for {
								var (
									Parser       = parser.NewParser(TaskBuffer)
									CommandID    = 0
									SubCommandID = 0
								)

								Parser.SetBigEndian(false)

								Parser.ParseInt32()
								Parser.ParseInt32()

								CommandID = Parser.ParseInt32()

								if CommandID != agent.COMMAND_PIVOT {
									CallbackSizes[int64(PivotAgentID)] = append(CallbackSizes[job[j].Data[1].(int64)], TaskBuffer...)
									break
								}

								/* get an instance of the pivot */
								PivotInstance = Teamserver.AgentInstance(PivotAgentID)
								if PivotInstance != nil {
									break
								}

								/* parse the task from the parser */
								TaskBuffer = Parser.ParseBytes()

								/* create a new parse for the parsed task */
								Parser = parser.NewParser(TaskBuffer)
								Parser.DecryptBuffer(PivotInstance.Encryption.AESKey, PivotInstance.Encryption.AESIv)

								if Parser.Length() >= 4 {
									SubCommandID = Parser.ParseInt32()
									SubCommandID = int(bits.ReverseBytes32(uint32(SubCommandID)))

									if SubCommandID == agent.AGENT_PIVOT_SMB_COMMAND {
										PivotAgentID = Parser.ParseInt32()
										PivotAgentID = int(bits.ReverseBytes32(uint32(PivotAgentID)))

										TaskBuffer = Parser.ParseBytes()
										continue

									} else {

										CallbackSizes[int64(PivotAgentID)] = append(CallbackSizes[job[j].Data[1].(int64)], TaskBuffer...)

										break
									}
								}

							}

						}
						break

					case agent.COMMAND_SOCKET:

						/* just send it to the agent and don't let the operator know or else this can be spamming the console lol */
						if job[j].Data[0] == agent.SOCKET_COMMAND_CLOSE || job[j].Data[0] == agent.SOCKET_COMMAND_READ_WRITE || job[j].Data[0] == agent.SOCKET_COMMAND_CONNECT {
							payload = agent.BuildPayloadMessage([]agent.Job{job[j]}, Agent.Encryption.AESKey, Agent.Encryption.AESIv)
						}

						break

					default:

						/* build the task payload */
						payload = agent.BuildPayloadMessage([]agent.Job{job[j]}, Agent.Encryption.AESKey, Agent.Encryption.AESIv)

						/* add the size of the task to the callback size */
						CallbackSizes[int64(Header.AgentID)] = append(CallbackSizes[int64(Header.AgentID)], payload...)

						break

					}
				}

				for agentID, buffer := range CallbackSizes {
					Agent = Teamserver.AgentInstance(int(agentID))
					if Agent != nil {
						Teamserver.AgentCallbackSize(Agent, len(buffer))
					}
				}

				CallbackSizes = nil
			}
		}

	} else {
		logger.Debug("Agent does not exists. hope this is a register request")

		var (
			Command = Header.Data.ParseInt32()
		)

		/* TODO: rework this. */
		if Command == agent.DEMON_INIT {
			Agent = agent.ParseResponse(Header.AgentID, Header.Data)
			if Agent == nil {
				return Response, false
			}

			go Agent.BackgroundUpdateLastCallbackUI(Teamserver)

			Agent.TaskedOnce = false
			Agent.Info.MagicValue = Header.MagicValue
			Agent.Info.Listener = nil /* TODO: pass here the listener instance */

			Teamserver.AgentAdd(Agent)
			Teamserver.AgentSendNotify(Agent)

			Packer = packer.NewPacker(Agent.Encryption.AESKey, Agent.Encryption.AESIv)
			Packer.AddUInt32(uint32(Header.AgentID))

			Build = Packer.Build()

			logger.Debug(fmt.Sprintf("%x", Build))

			_, err = Response.Write(Build)
			if err != nil {
				logger.Error(err)
				return Response, false
			}

			logger.Debug("Finished request")
		} else {
			logger.Debug("Is not register request. bye...")
			return Response, false
		}
	}

	return Response, true
}

func handleServiceAgent(Teamserver agent.TeamServer, Header agent.Header) (bytes.Buffer, bool) {

	var (
		Response  bytes.Buffer
		AgentData any
		Agent     *agent.Agent
		Task      []byte
		err       error
	)

	/* search if a service 3rd party agent was registered with this MagicValue */
	if !Teamserver.ServiceAgentExist(Header.MagicValue) {
		return Response, false
	}

	Agent = Teamserver.AgentInstance(Header.AgentID)
	if Agent != nil {
		AgentData = Agent.ToMap()
		go Agent.BackgroundUpdateLastCallbackUI(Teamserver)
	}

	Task = Teamserver.ServiceAgent(Header.MagicValue).SendResponse(AgentData, Header)
	logger.Debug("Response:\n", hex.Dump(Task))

	_, err = Response.Write(Task)
	if err != nil {
		return Response, false
	}

	return Response, true
}
