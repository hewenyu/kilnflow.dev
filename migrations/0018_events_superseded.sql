-- 判断事件是否已经过时：查找同一个 PR、同一个 ref 的 push 更晚收到的事件。
CREATE INDEX events_pull_request ON events (((payload->'repository'->>'id')), ((payload->'pull_request'->>'number')), received_at)
  WHERE event_type = 'pull_request';
CREATE INDEX events_push ON events (((payload->'repository'->>'id')), ((payload->>'ref')), received_at)
  WHERE event_type = 'push';
