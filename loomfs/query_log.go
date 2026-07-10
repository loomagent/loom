package loomfs

// LoadQueryRecords 读 workspace 的全量查询记录(queries 视图)。
// 查询写入/去重一律走 TurnSession(ObserveSearch/HasQuery)——
// workspace session 是 executor 运行契约,无 session 的旁路写入器已删除。
func LoadQueryRecords(runDir string) ([]QueryRecord, error) {
	ws, err := OpenWorkspace(runDir)
	if err != nil {
		return nil, err
	}
	snapshot, err := ws.LoadSnapshot(nilContext(), SnapshotOptions{})
	if err != nil {
		return nil, err
	}
	return snapshot.Queries, nil
}
