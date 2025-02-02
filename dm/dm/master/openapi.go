// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

// this file implement all of the APIs of the DataMigration service.

package master

import (
	"context"
	"fmt"
	"net/http"

	"github.com/deepmap/oapi-codegen/pkg/middleware"
	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"

	"github.com/pingcap/ticdc/dm/checker"
	"github.com/pingcap/ticdc/dm/dm/config"
	"github.com/pingcap/ticdc/dm/dm/ctl/common"
	"github.com/pingcap/ticdc/dm/dm/master/scheduler"
	"github.com/pingcap/ticdc/dm/dm/pb"
	"github.com/pingcap/ticdc/dm/openapi"
	"github.com/pingcap/ticdc/dm/pkg/conn"
	"github.com/pingcap/ticdc/dm/pkg/log"
	"github.com/pingcap/ticdc/dm/pkg/terror"
	"github.com/pingcap/ticdc/dm/pkg/utils"
)

const (
	docJSONBasePath = "/api/v1/dm.json"
)

// redirectRequestToLeaderMW a middleware auto redirect request to leader.
// because the leader has some data in memory, only the leader can process the request.
func (s *Server) redirectRequestToLeaderMW() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			ctx2 := ctx.Request().Context()
			isLeader, _ := s.isLeaderAndNeedForward(ctx2)
			if isLeader {
				return next(ctx)
			}
			// nolint:dogsled
			_, _, leaderOpenAPIAddr, err := s.election.LeaderInfo(ctx2)
			if err != nil {
				return err
			}
			return ctx.Redirect(http.StatusTemporaryRedirect, fmt.Sprintf("http://%s%s", leaderOpenAPIAddr, ctx.Request().RequestURI))
		}
	}
}

// InitOpenAPIHandles init openapi handlers.
func (s *Server) InitOpenAPIHandles() error {
	swagger, err := openapi.GetSwagger()
	if err != nil {
		return err
	}
	e := echo.New()
	// inject err handler
	e.HTTPErrorHandler = terrorHTTPErrorHandler
	// middlewares
	logger := log.L().WithFields(zap.String("component", "openapi")).Logger
	// set logger
	e.Use(openapi.ZapLogger(logger))
	e.Use(echomiddleware.Recover())
	e.Use(s.redirectRequestToLeaderMW())
	// disables swagger server name validation. it seems to work poorly
	swagger.Servers = nil
	// use our validation middleware to check all requests against the OpenAPI schema.
	e.Use(middleware.OapiRequestValidator(swagger))
	openapi.RegisterHandlers(e, s)
	s.echo = e
	return nil
}

// GetDocJSON url is:(GET /api/v1/dm.json).
func (s *Server) GetDocJSON(ctx echo.Context) error {
	swaggerJSON, err := openapi.GetSwaggerJSON()
	if err != nil {
		return err
	}
	return ctx.JSONBlob(200, swaggerJSON)
}

// GetDocHTML url is:(GET /api/v1/docs).
func (s *Server) GetDocHTML(ctx echo.Context) error {
	html, err := openapi.GetSwaggerHTML(openapi.NewSwaggerConfig(docJSONBasePath, ""))
	if err != nil {
		return err
	}
	return ctx.HTML(http.StatusOK, html)
}

// DMAPIGetClusterMasterList get cluster master node list url is:(GET /api/v1/cluster/masters).
func (s *Server) DMAPIGetClusterMasterList(ctx echo.Context) error {
	newCtx := ctx.Request().Context()
	memberMasters, err := s.listMemberMaster(newCtx, nil)
	if err != nil {
		return err
	}
	masterCnt := len(memberMasters.Master.Masters)
	masters := make([]openapi.ClusterMaster, masterCnt)
	for idx, master := range memberMasters.Master.Masters {
		masters[idx] = openapi.ClusterMaster{
			Name:   master.GetName(),
			Alive:  master.GetAlive(),
			Addr:   master.GetPeerURLs()[0],
			Leader: master.GetName() == s.cfg.Name, // only leader can handle request
		}
	}
	resp := &openapi.GetClusterMasterListResponse{Total: masterCnt, Data: masters}
	return ctx.JSON(http.StatusOK, resp)
}

// DMAPIOfflineMasterNode offline master node url is: (DELETE /api/v1/cluster/masters/{master-name}).
func (s *Server) DMAPIOfflineMasterNode(ctx echo.Context, masterName string) error {
	newCtx := ctx.Request().Context()
	if err := s.deleteMasterByName(newCtx, masterName); err != nil {
		return err
	}
	return ctx.NoContent(http.StatusNoContent)
}

// DMAPIGetClusterWorkerList get cluster worker node list url is: (GET /api/v1/cluster/workers).
func (s *Server) DMAPIGetClusterWorkerList(ctx echo.Context) error {
	memberWorkers := s.listMemberWorker(nil)
	workerCnt := len(memberWorkers.Worker.Workers)
	workers := make([]openapi.ClusterWorker, workerCnt)
	for idx, worker := range memberWorkers.Worker.Workers {
		workers[idx] = openapi.ClusterWorker{
			Name:            worker.GetName(),
			Addr:            worker.GetAddr(),
			BoundSourceName: worker.GetSource(),
			BoundStage:      worker.GetStage(),
		}
	}
	resp := &openapi.GetClusterWorkerListResponse{Total: workerCnt, Data: workers}
	return ctx.JSON(http.StatusOK, resp)
}

// DMAPIOfflineWorkerNode offline worker node url is: (DELETE /api/v1/cluster/workers/{worker-name}).
func (s *Server) DMAPIOfflineWorkerNode(ctx echo.Context, workerName string) error {
	if err := s.scheduler.RemoveWorker(workerName); err != nil {
		return err
	}
	return ctx.NoContent(http.StatusNoContent)
}

// DMAPICreateSource url is:(POST /api/v1/sources).
func (s *Server) DMAPICreateSource(ctx echo.Context) error {
	var createSourceReq openapi.Source
	if err := ctx.Bind(&createSourceReq); err != nil {
		return err
	}
	cfg := modelToSourceCfg(createSourceReq)
	if err := checkAndAdjustSourceConfigFunc(ctx.Request().Context(), cfg); err != nil {
		return err
	}
	if err := s.scheduler.AddSourceCfg(cfg); err != nil {
		return err
	}
	return ctx.JSON(http.StatusCreated, createSourceReq)
}

// DMAPIGetSourceList url is:(GET /api/v1/sources).
func (s *Server) DMAPIGetSourceList(ctx echo.Context, params openapi.DMAPIGetSourceListParams) error {
	sourceMap := s.scheduler.GetSourceCfgs()
	sourceList := []openapi.Source{}
	for key := range sourceMap {
		sourceList = append(sourceList, sourceCfgToModel(sourceMap[key]))
	}
	// fill status
	if params.WithStatus != nil && *params.WithStatus {
		nexCtx := ctx.Request().Context()
		for idx := range sourceList {
			sourceStatusList, err := s.getSourceStatusListFromWorker(nexCtx, sourceList[idx].SourceName)
			if err != nil {
				return err
			}
			sourceList[idx].StatusList = &sourceStatusList
		}
	}
	resp := openapi.GetSourceListResponse{Total: len(sourceList), Data: sourceList}
	return ctx.JSON(http.StatusOK, resp)
}

// DMAPIDeleteSource url is:(DELETE /api/v1/sources).
func (s *Server) DMAPIDeleteSource(ctx echo.Context, sourceName string, params openapi.DMAPIDeleteSourceParams) error {
	// force means delete source and stop all task of this source
	if params.Force != nil && *params.Force {
		// TODO(ehco) stop task concurrently
		newCtx := ctx.Request().Context()
		for _, taskName := range s.scheduler.GetTaskNameListBySourceName(sourceName) {
			if _, err := s.OperateTask(newCtx, &pb.OperateTaskRequest{
				Op:      pb.TaskOp_Stop,
				Name:    taskName,
				Sources: []string{sourceName},
			}); err != nil {
				return terror.ErrOpenAPICommonError.Delegate(err, "failed to stop source related task %s", taskName)
			}
		}
	}
	if err := s.scheduler.RemoveSourceCfg(sourceName); err != nil {
		return err
	}
	return ctx.NoContent(http.StatusNoContent)
}

// DMAPIStartRelay url is:(POST /api/v1/sources/{source-id}/relay).
func (s *Server) DMAPIStartRelay(ctx echo.Context, sourceName string) error {
	var req openapi.StartRelayRequest
	if err := ctx.Bind(&req); err != nil {
		return err
	}
	sourceCfg := s.scheduler.GetSourceCfgByID(sourceName)
	if sourceCfg == nil {
		return terror.ErrSchedulerSourceCfgNotExist.Generate(sourceName)
	}
	needUpdate := false
	// update relay related in source cfg
	if req.RelayBinlogName != nil && sourceCfg.RelayBinLogName != *req.RelayBinlogName {
		sourceCfg.RelayBinLogName = *req.RelayBinlogName
		needUpdate = true
	}
	if req.RelayBinlogGtid != nil && sourceCfg.RelayBinlogGTID != *req.RelayBinlogGtid {
		sourceCfg.RelayBinlogGTID = *req.RelayBinlogGtid
		needUpdate = true
	}
	if req.RelayDir != nil && sourceCfg.RelayDir != *req.RelayDir {
		sourceCfg.RelayDir = *req.RelayDir
		needUpdate = true
	}
	if needUpdate {
		// update current source relay config before start relay
		if err := s.scheduler.UpdateSourceCfg(sourceCfg); err != nil {
			return err
		}
	}
	return s.scheduler.StartRelay(sourceName, req.WorkerNameList)
}

// DMAPIStopRelay url is:(DELETE /api/v1/sources/{source-id}/relay).
func (s *Server) DMAPIStopRelay(ctx echo.Context, sourceName string) error {
	var req openapi.StopRelayRequest
	if err := ctx.Bind(&req); err != nil {
		return err
	}
	return s.scheduler.StopRelay(sourceName, req.WorkerNameList)
}

// DMAPIPauseRelay pause relay log function for the data source url is: (POST /api/v1/sources/{source-name}/pause-relay).
func (s *Server) DMAPIPauseRelay(ctx echo.Context, sourceName string) error {
	return s.scheduler.UpdateExpectRelayStage(pb.Stage_Paused, sourceName)
}

// DMAPIResumeRelay resume relay log function for the data source url is: (POST /api/v1/sources/{source-name}/resume-relay).
func (s *Server) DMAPIResumeRelay(ctx echo.Context, sourceName string) error {
	return s.scheduler.UpdateExpectRelayStage(pb.Stage_Running, sourceName)
}

func (s *Server) getBaseDBBySourceName(sourceName string) (*conn.BaseDB, error) {
	sourceCfg := s.scheduler.GetSourceCfgByID(sourceName)
	if sourceCfg == nil {
		return nil, terror.ErrSchedulerSourceCfgNotExist.Generate(sourceName)
	}
	dbCfg := sourceCfg.GenerateDBConfig()
	return conn.DefaultDBProvider.Apply(*dbCfg)
}

// DMAPIGetSourceSchemaList get source schema list url is: (GET /api/v1/sources/{source-name}/schemas).
func (s *Server) DMAPIGetSourceSchemaList(ctx echo.Context, sourceName string) error {
	baseDB, err := s.getBaseDBBySourceName(sourceName)
	if err != nil {
		return err
	}
	defer baseDB.Close()
	schemaList, err := utils.GetSchemaList(ctx.Request().Context(), baseDB.DB)
	if err != nil {
		return err
	}
	return ctx.JSON(http.StatusOK, schemaList)
}

// DMAPIGetSourceTableList get source table list url is: (GET /api/v1/sources/{source-name}/schemas/{schema-name}).
func (s *Server) DMAPIGetSourceTableList(ctx echo.Context, sourceName string, schemaName string) error {
	baseDB, err := s.getBaseDBBySourceName(sourceName)
	if err != nil {
		return err
	}
	defer baseDB.Close()
	tableList, err := utils.GetTableList(ctx.Request().Context(), baseDB.DB, schemaName)
	if err != nil {
		return err
	}
	return ctx.JSON(http.StatusOK, tableList)
}

func (s *Server) getSourceStatusListFromWorker(ctx context.Context, sourceName string) ([]openapi.SourceStatus, error) {
	workerStatusList := s.getStatusFromWorkers(ctx, []string{sourceName}, "", true)
	sourceStatusList := make([]openapi.SourceStatus, len(workerStatusList))
	for i, workerStatus := range workerStatusList {
		if workerStatus == nil {
			// this should not happen unless the rpc in the worker server has been modified
			return nil, terror.ErrOpenAPICommonError.New("worker's query-status response is nil")
		}
		sourceStatus := openapi.SourceStatus{SourceName: sourceName, WorkerName: workerStatus.SourceStatus.Worker}
		if !workerStatus.Result {
			sourceStatus.ErrorMsg = &workerStatus.Msg
		} else if relayStatus := workerStatus.SourceStatus.GetRelayStatus(); relayStatus != nil {
			sourceStatus.RelayStatus = &openapi.RelayStatus{
				MasterBinlog:       relayStatus.MasterBinlog,
				MasterBinlogGtid:   relayStatus.MasterBinlogGtid,
				RelayBinlogGtid:    relayStatus.RelayBinlogGtid,
				RelayCatchUpMaster: relayStatus.RelayCatchUpMaster,
				RelayDir:           relayStatus.RelaySubDir,
				Stage:              relayStatus.Stage.String(),
			}
		}
		sourceStatusList[i] = sourceStatus
	}
	return sourceStatusList, nil
}

// DMAPIGetSourceStatus url is: (GET /api/v1/sources/{source-id}/status).
func (s *Server) DMAPIGetSourceStatus(ctx echo.Context, sourceName string) error {
	sourceCfg := s.scheduler.GetSourceCfgByID(sourceName)
	if sourceCfg == nil {
		return terror.ErrSchedulerSourceCfgNotExist.Generate(sourceName)
	}
	var resp openapi.GetSourceStatusResponse
	worker := s.scheduler.GetWorkerBySource(sourceName)
	// current this source not bound to any worker
	if worker == nil {
		resp.Data = append(resp.Data, openapi.SourceStatus{SourceName: sourceName})
		resp.Total = len(resp.Data)
		return ctx.JSON(http.StatusOK, resp)
	}
	sourceStatusList, err := s.getSourceStatusListFromWorker(ctx.Request().Context(), sourceName)
	if err != nil {
		return err
	}
	resp.Data = append(resp.Data, sourceStatusList...)
	resp.Total = len(resp.Data)
	return ctx.JSON(http.StatusOK, resp)
}

// DMAPITransferSource transfer source  another free worker url is: (POST /api/v1/sources/{source-name}/transfer).
func (s *Server) DMAPITransferSource(ctx echo.Context, sourceName string) error {
	var req openapi.WorkerNameRequest
	if err := ctx.Bind(&req); err != nil {
		return err
	}
	return s.scheduler.TransferSource(sourceName, req.WorkerName)
}

// DMAPIStartTask url is:(POST /api/v1/tasks).
func (s *Server) DMAPIStartTask(ctx echo.Context) error {
	var req openapi.CreateTaskRequest
	if err := ctx.Bind(&req); err != nil {
		return err
	}
	task := &req.Task
	if err := task.Adjust(); err != nil {
		return err
	}
	// prepare target db config
	newCtx := ctx.Request().Context()
	toDBCfg := config.GetTargetDBCfgFromOpenAPITask(task)
	if adjustDBErr := adjustTargetDB(newCtx, toDBCfg); adjustDBErr != nil {
		return terror.WithClass(adjustDBErr, terror.ClassDMMaster)
	}
	// prepare source db config source name -> source config
	sourceCfgMap := make(map[string]*config.SourceConfig)
	for _, cfg := range task.SourceConfig.SourceConf {
		if sourceCfg := s.scheduler.GetSourceCfgByID(cfg.SourceName); sourceCfg != nil {
			sourceCfgMap[cfg.SourceName] = sourceCfg
		} else {
			return terror.ErrOpenAPITaskSourceNotFound.Generatef("source name %s", cfg.SourceName)
		}
	}
	// generate sub task configs
	subTaskConfigList, err := config.OpenAPITaskToSubTaskConfigs(task, toDBCfg, sourceCfgMap)
	if err != nil {
		return err
	}
	// check subtask config
	subTaskConfigPList := make([]*config.SubTaskConfig, len(subTaskConfigList))
	for i := range subTaskConfigList {
		subTaskConfigPList[i] = &subTaskConfigList[i]
	}
	if err = checker.CheckSyncConfigFunc(newCtx, subTaskConfigPList,
		common.DefaultErrorCnt, common.DefaultWarnCnt); err != nil {
		return terror.WithClass(err, terror.ClassDMMaster)
	}
	// specify only start task on partial sources
	var needStartSubTaskList []config.SubTaskConfig
	if req.SourceNameList != nil {
		// source name -> sub task config
		subTaskCfgM := make(map[string]*config.SubTaskConfig, len(subTaskConfigList))
		for idx := range subTaskConfigList {
			cfg := subTaskConfigList[idx]
			subTaskCfgM[cfg.SourceID] = &cfg
		}
		for _, sourceName := range *req.SourceNameList {
			subTaskCfg, ok := subTaskCfgM[sourceName]
			if !ok {
				return terror.ErrOpenAPITaskSourceNotFound.Generatef("source name %s", sourceName)
			}
			needStartSubTaskList = append(needStartSubTaskList, *subTaskCfg)
		}
	} else {
		needStartSubTaskList = subTaskConfigList
	}
	// end all pre-check, start to create task
	var (
		latched = false
		release scheduler.ReleaseFunc
	)
	if req.RemoveMeta {
		// use same latch for remove-meta and start-task
		release, err = s.scheduler.AcquireSubtaskLatch(task.Name)
		if err != nil {
			return terror.ErrSchedulerLatchInUse.Generate("RemoveMeta", task.Name)
		}
		defer release()
		latched = true
		err = s.removeMetaData(newCtx, task.Name, *task.MetaSchema, toDBCfg)
		if err != nil {
			return terror.Annotate(err, "while removing metadata")
		}
	}
	err = s.scheduler.AddSubTasks(latched, needStartSubTaskList...)
	if err != nil {
		return err
	}
	if release != nil {
		release()
	}
	return ctx.JSON(http.StatusCreated, task)
}

// DMAPIDeleteTask url is:(DELETE /api/v1/tasks).
func (s *Server) DMAPIDeleteTask(ctx echo.Context, taskName string, params openapi.DMAPIDeleteTaskParams) error {
	var sourceList []string
	if params.SourceNameList != nil {
		sourceList = *params.SourceNameList
	} else {
		sourceList = s.getTaskResources(taskName)
	}
	if len(sourceList) == 0 {
		return terror.ErrSchedulerTaskNotExist.Generate(taskName)
	}
	if err := s.scheduler.RemoveSubTasks(taskName, sourceList...); err != nil {
		return err
	}
	return ctx.NoContent(http.StatusNoContent)
}

// DMAPIGetTaskList url is:(GET /api/v1/tasks).
func (s *Server) DMAPIGetTaskList(ctx echo.Context) error {
	// get sub task config by task name task name->source name->subtask config
	subTaskConfigMap := s.scheduler.GetSubTaskCfgs()
	taskList := config.SubTaskConfigsToOpenAPITask(subTaskConfigMap)
	resp := openapi.GetTaskListResponse{Total: len(taskList), Data: taskList}
	return ctx.JSON(http.StatusOK, resp)
}

// DMAPIGetTaskStatus url is:(GET /api/v1/tasks/{task-name}/status).
func (s *Server) DMAPIGetTaskStatus(ctx echo.Context, taskName string, params openapi.DMAPIGetTaskStatusParams) error {
	// todo support params
	// 1. get task source list from scheduler
	sourceList := s.getTaskResources(taskName)
	if len(sourceList) == 0 {
		return terror.ErrSchedulerTaskNotExist.Generate(taskName)
	}
	// 2. get status from workers
	workerStatusList := s.getStatusFromWorkers(ctx.Request().Context(), sourceList, taskName, true)
	subTaskStatusList := make([]openapi.SubTaskStatus, len(workerStatusList))
	for i, workerStatus := range workerStatusList {
		if workerStatus == nil || workerStatus.SourceStatus == nil {
			// this should not happen unless the rpc in the worker server has been modified
			return terror.ErrOpenAPICommonError.New("worker's query-status response is nil")
		}
		sourceStatus := workerStatus.SourceStatus
		// find right task name
		var subTaskStatus *pb.SubTaskStatus
		for _, cfg := range workerStatus.SubTaskStatus {
			if cfg.Name == taskName {
				subTaskStatus = cfg
			}
		}
		if subTaskStatus == nil {
			// this may not happen
			return terror.ErrOpenAPICommonError.Generatef("can not find subtask status task name: %s.", taskName)
		}
		openapiSubTaskStatus := openapi.SubTaskStatus{
			Name:                taskName,
			SourceName:          sourceStatus.GetSource(),
			WorkerName:          sourceStatus.GetWorker(),
			Stage:               subTaskStatus.GetStage().String(),
			Unit:                subTaskStatus.GetUnit().String(),
			UnresolvedDdlLockId: &subTaskStatus.UnresolvedDDLLockID,
		}
		// add load status
		if loadS := subTaskStatus.GetLoad(); loadS != nil {
			openapiSubTaskStatus.LoadStatus = &openapi.LoadStatus{
				FinishedBytes:  loadS.FinishedBytes,
				MetaBinlog:     loadS.MetaBinlog,
				MetaBinlogGtid: loadS.MetaBinlogGTID,
				Progress:       loadS.Progress,
				TotalBytes:     loadS.TotalBytes,
			}
		}
		// add syncer status
		if syncerS := subTaskStatus.GetSync(); syncerS != nil {
			openapiSubTaskStatus.SyncStatus = &openapi.SyncStatus{
				BinlogType:          syncerS.GetBinlogType(),
				BlockingDdls:        syncerS.GetBlockingDDLs(),
				MasterBinlog:        syncerS.GetMasterBinlog(),
				MasterBinlogGtid:    syncerS.GetMasterBinlogGtid(),
				RecentTps:           syncerS.RecentTps,
				SecondsBehindMaster: syncerS.SecondsBehindMaster,
				Synced:              syncerS.Synced,
				SyncerBinlog:        syncerS.SyncerBinlog,
				SyncerBinlogGtid:    syncerS.SyncerBinlogGtid,
				TotalEvents:         syncerS.TotalEvents,
				TotalTps:            syncerS.TotalTps,
			}
			if unResolvedGroups := syncerS.GetUnresolvedGroups(); len(unResolvedGroups) > 0 {
				openapiSubTaskStatus.SyncStatus.UnresolvedGroups = make([]openapi.ShardingGroup, len(unResolvedGroups))
				for i, unResolvedGroup := range unResolvedGroups {
					openapiSubTaskStatus.SyncStatus.UnresolvedGroups[i] = openapi.ShardingGroup{
						DdlList:       unResolvedGroup.DDLs,
						FirstLocation: unResolvedGroup.FirstLocation,
						Synced:        unResolvedGroup.Synced,
						Target:        unResolvedGroup.Target,
						Unsynced:      unResolvedGroup.Unsynced,
					}
				}
			}
		}
		subTaskStatusList[i] = openapiSubTaskStatus
	}
	resp := openapi.GetTaskStatusResponse{Total: len(subTaskStatusList), Data: subTaskStatusList}
	return ctx.JSON(http.StatusOK, resp)
}

// DMAPPauseTask pause task url is: (POST /api/v1/tasks/{task-name}/pause).
func (s *Server) DMAPPauseTask(ctx echo.Context, taskName string) error {
	return nil
}

// DMAPIResumeTask resume task url is: (POST /api/v1/tasks/{task-name}/resume).
func (s *Server) DMAPIResumeTask(ctx echo.Context, taskName string) error {
	return nil
}

// DMAPIGetTaskSourceSchemaList get task source schema list url is: (GET /api/v1/tasks/{task-name}/sources/{source-name}/schemas).
func (s *Server) DMAPIGetTaskSourceSchemaList(ctx echo.Context, taskName string, sourceName string) error {
	return nil
}

// DMAPIGetTaskSchemaStructure get task source schema structure url is: (GET /api/v1/tasks/{task-name}/sources/{source-name}/schemas/{schema-name}).
func (s *Server) DMAPIGetTaskSchemaStructure(ctx echo.Context, taskName string, sourceName string, schemaName string) error {
	return nil
}

// DMAPIGetTaskSourceTableList get task source table list url is: (GET /api/v1/tasks/{task-name}/sources/{source-name}/schemas/{schema-name}).
func (s *Server) DMAPIGetTaskSourceTableList(ctx echo.Context, taskName string, sourceName string, schemaName string) error {
	return nil
}

// DMAPIDeleteTaskSourceTableStructure delete task source table structure url is: (DELETE /api/v1/tasks/{task-name}/sources/{source-name}/schemas/{schema-name}/{table-name}).
func (s *Server) DMAPIDeleteTaskSourceTableStructure(ctx echo.Context, taskName string, sourceName string, schemaName string, tableName string) error {
	return nil
}

// DMAPIGetTaskSourceTableStructure get task source table structure url is: (GET /api/v1/tasks/{task-name}/sources/{source-name}/schemas/{schema-name}/{table-name}).
func (s *Server) DMAPIGetTaskSourceTableStructure(ctx echo.Context, taskName string, sourceName string, schemaName string, tableName string) error {
	return nil
}

// DMAPIOperateTaskSourceTableStructure operate task source table structure url is: (PUT /api/v1/tasks/{task-name}/sources/{source-name}/schemas/{schema-name}/{table-name}).
func (s *Server) DMAPIOperateTaskSourceTableStructure(ctx echo.Context, taskName string, sourceName string, schemaName string, tableName string) error {
	return nil
}

func terrorHTTPErrorHandler(err error, c echo.Context) {
	var code int
	var msg string
	if tErr, ok := err.(*terror.Error); ok {
		code = int(tErr.Code())
		msg = tErr.Error()
	} else {
		msg = err.Error()
	}
	if sendErr := sendHTTPErrorResp(c, code, msg); sendErr != nil {
		c.Logger().Error(sendErr)
	}
}

func sendHTTPErrorResp(ctx echo.Context, code int, message string) error {
	err := openapi.ErrorWithMessage{ErrorMsg: message, ErrorCode: code}
	return ctx.JSON(http.StatusBadRequest, err)
}

func sourceCfgToModel(cfg *config.SourceConfig) openapi.Source {
	// PM's requirement, we always return obfuscated password to user
	source := openapi.Source{
		EnableGtid: cfg.EnableGTID,
		Host:       cfg.From.Host,
		Password:   "******",
		Port:       cfg.From.Port,
		SourceName: cfg.SourceID,
		User:       cfg.From.User,
		Purge: &openapi.Purge{
			Expires:     &cfg.Purge.Expires,
			Interval:    &cfg.Purge.Interval,
			RemainSpace: &cfg.Purge.RemainSpace,
		},
	}
	if cfg.From.Security != nil {
		// NOTE we don't return security content here, because we don't want to expose it to the user.
		var certAllowedCn []string
		certAllowedCn = append(certAllowedCn, cfg.From.Security.CertAllowedCN...)
		source.Security = &openapi.Security{CertAllowedCn: &certAllowedCn}
	}
	return source
}

func modelToSourceCfg(source openapi.Source) *config.SourceConfig {
	cfg := config.NewSourceConfig()
	from := config.DBConfig{
		Host:     source.Host,
		Port:     source.Port,
		User:     source.User,
		Password: source.Password,
	}
	if source.Security != nil {
		from.Security = &config.Security{
			SSLCABytes:   []byte(source.Security.SslCaContent),
			SSLKEYBytes:  []byte(source.Security.SslKeyContent),
			SSLCertBytes: []byte(source.Security.SslCertContent),
		}
		if source.Security.CertAllowedCn != nil {
			from.Security.CertAllowedCN = *source.Security.CertAllowedCn
		}
	}
	cfg.From = from
	cfg.EnableGTID = source.EnableGtid
	cfg.SourceID = source.SourceName
	if purge := source.Purge; purge != nil {
		if purge.Expires != nil {
			cfg.Purge.Expires = *purge.Expires
		}
		if purge.Interval != nil {
			cfg.Purge.Interval = *purge.Interval
		}
		if purge.RemainSpace != nil {
			cfg.Purge.RemainSpace = *purge.RemainSpace
		}
	}
	return cfg
}
