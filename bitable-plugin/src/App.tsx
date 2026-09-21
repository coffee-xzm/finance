import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  activeSelection,
  attachmentUrl,
  cellText,
  fetchRecordIds,
  fieldIdMap,
  findSlotFields,
  instanceNoOf,
  listAttachmentFields,
  listTables,
  listViews,
  readRecords,
  toAttachmentRefs,
  toNamingFields,
  type SelectedRecord,
  type SlotFields,
  type TableOption,
  type ViewOption,
} from './api/bitable.js';
import {
  DEFAULT_TEMPLATE,
  SLOT_FIELD,
  SLOT_LABEL,
  planRows,
  type NamingOptions,
  type PlanResult,
  type Slot,
} from './core/naming.js';
import { downloadAll, transportName, type DownloadFailure } from './core/download.js';
import { buildManifest, buildReadme } from './core/manifest.js';
import { csvBytes, makeZip, saveBlob } from './core/zip.js';
import './style.css';

const SLOTS: Slot[] = ['invoice', 'order', 'payment'];

/** 三列字段名与 canonical 名不一致时给出提示，避免"以为勾了其实没勾到"。 */
function slotHint(slot: Slot, mappedName: string | undefined): string | undefined {
  if (!mappedName) return '未找到该附件字段（请手动指定）';
  if (mappedName !== SLOT_FIELD[slot]) return `列名是「${mappedName}」`;
  return undefined;
}

export default function App() {
  const [tables, setTables] = useState<TableOption[]>([]);
  const [tableId, setTableId] = useState('');
  const [views, setViews] = useState<ViewOption[]>([]);
  const [viewId, setViewId] = useState('');

  const [attFields, setAttFields] = useState<{ id: string; name: string }[]>([]);
  const [slots, setSlots] = useState<SlotFields>({});

  const [template, setTemplate] = useState(DEFAULT_TEMPLATE);
  const [groupBy, setGroupBy] = useState<'department' | 'month' | 'none'>('department');

  const [records, setRecords] = useState<SelectedRecord[]>([]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [plan, setPlan] = useState<PlanResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState('');
  const [errors, setErrors] = useState<string[]>([]);
  const [failures, setFailures] = useState<DownloadFailure[]>([]);
  const [progress, setProgress] = useState({ done: 0, total: 0, current: '' });
  const stopRef = useRef(false);

  // ── 初始化：列出数据表，默认选中当前打开的表/视图 ────────────────
  useEffect(() => {
    void (async () => {
      try {
        const [ts, sel] = await Promise.all([listTables(), activeSelection()]);
        setTables(ts);
        const first = ts.find((t) => t.id === sel.tableId) ?? ts[0];
        if (first) setTableId(first.id);
      } catch (err) {
        setErrors([`读取数据表失败：${String(err)}`]);
      }
    })();
  }, []);

  // ── 换表：列视图 + 找附件字段 ────────────────────────────────
  useEffect(() => {
    if (!tableId) return;
    void (async () => {
      setErrors([]);
      setRecords([]);
      setPlan(null);
      try {
        const [vs, fields, found] = await Promise.all([
          listViews(tableId),
          listAttachmentFields(tableId),
          findSlotFields(tableId),
        ]);
        setViews(vs);
        setAttFields(fields);
        setSlots(found);
        const sel = await activeSelection();
        const v = vs.find((x) => x.id === sel.viewId) ?? vs[0];
        setViewId(v?.id ?? '');
      } catch (err) {
        setErrors([`读取视图/字段失败：${String(err)}`]);
      }
    })();
  }, [tableId]);

  const namingOpts: NamingOptions = useMemo(
    () => ({ template, groupBy }),
    [template, groupBy],
  );

  const refreshPlan = useCallback(
    (recs: SelectedRecord[], keep: Set<string>) => {
      const chosen = recs.filter((r) => keep.has(r.recordId));
      setPlan(planRows(chosen, namingOpts));
    },
    [namingOpts],
  );

  // ── 读取记录（含附件 token）──────────────────────────────────
  const loadRecords = useCallback(async () => {
    if (!tableId || !viewId) return;
    setBusy(true);
    setErrors([]);
    setFailures([]);
    setStatus('正在读取记录…');
    try {
      const [allIds, names] = await Promise.all([
        fetchRecordIds(tableId, viewId),
        fieldIdMap(tableId),
      ]);
      if (allIds.length === 0) {
        setRecords([]);
        setPlan(null);
        setStatus('这个视图里没有记录。');
        return;
      }
      const values = await readRecords(tableId, allIds);
      const recs: SelectedRecord[] = values.map((v, i) => {
        const bySlot: SelectedRecord['attachments'] = {};
        for (const slot of SLOTS) {
          const fid = slots[slot];
          if (!fid) continue;
          const refs = toAttachmentRefs(v.fields?.[fid]);
          if (refs.length > 0) bySlot[slot] = refs;
        }
        const instanceNo = instanceNoOf(v.fields ?? {}, names);
        const recordId = allIds[i] ?? '';
        return {
          recordId,
          viewIndex: i + 1,
          fields: toNamingFields(v.fields ?? {}, names),
          attachments: bySlot,
          ...(instanceNo ? { instanceNo } : {}),
        };
      });
      setRecords(recs);
      const keep = new Set(recs.filter((r) => Object.keys(r.attachments).length > 0).map((r) => r.recordId));
      setSelected(keep);
      setPlan(planRows(recs.filter((r) => keep.has(r.recordId)), namingOpts));
      const withAtt = recs.filter((r) => Object.keys(r.attachments).length > 0).length;
      setStatus(`读到 ${recs.length} 条记录，其中 ${withAtt} 条有附件。`);
    } catch (err) {
      setErrors([`读取记录失败：${err instanceof Error ? err.message : String(err)}`]);
      setStatus('');
    } finally {
      setBusy(false);
    }
  }, [tableId, viewId, slots, namingOpts]);

  const toggle = (recordId: string) => {
    const next = new Set(selected);
    if (next.has(recordId)) next.delete(recordId);
    else next.add(recordId);
    setSelected(next);
    refreshPlan(records, next);
  };

  const toggleAll = (on: boolean) => {
    const next = on
      ? new Set(records.filter((r) => Object.keys(r.attachments).length > 0).map((r) => r.recordId))
      : new Set<string>();
    setSelected(next);
    refreshPlan(records, next);
  };

  // ── 导出：下载 → 打包 → 交给浏览器保存 ───────────────────────
  const doExport = useCallback(async () => {
    if (!plan || plan.files.length === 0) return;
    setBusy(true);
    setErrors([]);
    setFailures([]);
    stopRef.current = false;
    const byRecord = new Map(records.map((r) => [r.recordId, r]));
    const urlCache = new Map<string, Promise<string>>();
    try {
      setStatus('开始下载…');
      const outcome = await downloadAll({
        plans: plan.files,
        concurrency: 3,
        onProgress: (p) => setProgress({ done: p.done, total: p.total, current: p.current ?? '' }),
        shouldStop: () => stopRef.current,
        urlFor: (token) => {
          const file = plan.files.find((f) => f.token === token);
          if (!file) return Promise.reject(new Error('计划里找不到该 token'));
          const rec = byRecord.get(file.recordId);
          const fid = slots[file.slot];
          if (!rec || !fid) return Promise.reject(new Error('缺少记录或字段映射'));
          const key = `${file.recordId}#${file.slot}`;
          let p = urlCache.get(key);
          if (!p) {
            p = attachmentUrl(tableId, fid, file.recordId, token);
            urlCache.set(key, p);
          }
          return p;
        },
      });

      setFailures(outcome.failures);
      setStatus(`下载完成：成功 ${outcome.files.length} 个，失败 ${outcome.failures.length} 个。正在打包…`);

      const mtime = new Date();
      const entries = outcome.files.map((f) => ({
        dir: f.plan.dir,
        name: f.plan.name,
        data: f.data,
        mtime,
      }));
      const failedRows = outcome.failures.map((f) => ({ plan: f.plan, error: f.error }));
      const manifest = buildManifest([
        ...outcome.files.map((f) => ({ plan: f.plan, bytes: f.data.length })),
        ...failedRows,
      ]);
      entries.push({
        dir: '',
        name: 'manifest.csv',
        data: csvBytes(manifest),
        mtime,
      });
      const tableName = tables.find((t) => t.id === tableId)?.name ?? tableId;
      const viewName = views.find((v) => v.id === viewId)?.name ?? viewId;
      entries.push({
        dir: '',
        name: 'README.txt',
        data: new TextEncoder().encode(
          buildReadme({
            exportedAt: mtime.toLocaleString('zh-CN'),
            tableName,
            viewName,
            template,
            groupBy,
            rows: selected.size,
            files: outcome.files.length,
            failures: outcome.failures.length,
          }),
        ),
        mtime,
      });

      const zipData = await makeZip(entries, { level: 6 });
      const stamp = `${mtime.getFullYear()}${pad2(mtime.getMonth() + 1)}${pad2(mtime.getDate())}-${pad2(mtime.getHours())}${pad2(mtime.getMinutes())}`;
      saveBlob(zipData, `附件导出_${tableName}_${stamp}.zip`);
      setStatus(
        `已生成 zip：${outcome.files.length} 个文件${
          outcome.failures.length ? `，${outcome.failures.length} 个失败（见 manifest.csv）` : ''
        }。`,
      );
    } catch (err) {
      setErrors([`导出失败：${err instanceof Error ? err.message : String(err)}`]);
    } finally {
      setBusy(false);
    }
  }, [plan, records, slots, tableId, viewId, tables, views, template, groupBy, selected]);

  const totalBytes = plan?.totalBytes ?? 0;
  const selectedRows = records.filter((r) => selected.has(r.recordId));

  return (
    <div className="wrap">
      <header className="head">
        <strong>附件批量导出</strong>
        <span className="badge" title="本地打包：文件不经过任何服务器">
          本地打包 · {transportName()}
        </span>
      </header>

      <section className="card">
        <h4>数据源</h4>
        <label>
          数据表
          <select
            value={tableId}
            onChange={(e) => setTableId(e.target.value)}
            disabled={busy}
          >
            {tables.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          视图
          <select value={viewId} onChange={(e) => setViewId(e.target.value)} disabled={busy}>
            {views.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
              </option>
            ))}
          </select>
        </label>
      </section>

      <section className="card">
        <h4>附件字段</h4>
        {SLOTS.map((slot) => (
          <label key={slot}>
            {SLOT_LABEL[slot]}
            <select
              value={slots[slot] ?? ''}
              onChange={(e) => {
                const next = { ...slots };
                if (e.target.value) next[slot] = e.target.value;
                else delete next[slot];
                setSlots(next);
              }}
              disabled={busy}
            >
              <option value="">（不导出）</option>
              {attFields.map((f) => (
                <option key={f.id} value={f.id}>
                  {f.name}
                </option>
              ))}
            </select>
            {(() => {
              const mapped = attFields.find((f) => f.id === slots[slot])?.name;
              const hint = slotHint(slot, mapped);
              return hint ? <em className="hint">{hint}</em> : null;
            })()}
          </label>
        ))}
      </section>

      <section className="card">
        <h4>命名与分组</h4>
        <label>
          命名模板
          <input
            value={template}
            onChange={(e) => setTemplate(e.target.value)}
            disabled={busy}
            spellCheck={false}
          />
        </label>
        <p className="hint">
          可用变量：{' '}
          <code>
            {'{部门} {购买人} {日期} {销方} {金额} {槽位} {序号} {发票号码后6位}'}
          </code>
          （分组已包含的维度会自动从文件名去掉）
        </p>
        <label>
          文件夹分类
          <select
            value={groupBy}
            onChange={(e) => setGroupBy(e.target.value as typeof groupBy)}
            disabled={busy}
          >
            <option value="department">按物资所属部门</option>
            <option value="month">按月份（图读日期）</option>
            <option value="none">不分类</option>
          </select>
        </label>
      </section>

      <div className="actions">
        <button onClick={() => void loadRecords()} disabled={busy || !viewId}>
          读取记录
        </button>
        <button
          className="primary"
          onClick={() => void doExport()}
          disabled={busy || !plan || plan.files.length === 0}
        >
          {busy && progress.total > 0
            ? `下载中 ${progress.done}/${progress.total}`
            : `下载所选记录（${plan?.files.length ?? 0} 个文件）`}
        </button>
        <button onClick={() => setSelected(new Set())} disabled={busy || selected.size === 0}>
          清空选择
        </button>
      </div>

      <div className="status">
        {status}
        {totalBytes > 0 ? ` 预计 ${(totalBytes / 1024 / 1024).toFixed(1)} MB` : ''}
        {progress.current ? ` · ${progress.current}` : ''}
      </div>

      {errors.length > 0 && (
        <ul className="errors">
          {errors.map((e) => (
            <li key={e}>{e}</li>
          ))}
        </ul>
      )}

      {plan && plan.warnings.length > 0 && (
        <details className="warnbox" open>
          <summary>{plan.warnings.length} 条提示</summary>
          <ul>
            {plan.warnings.slice(0, 50).map((w, i) => (
              <li key={`${w.recordId}-${i}`}>
                <code>{w.code}</code> {w.message}
              </li>
            ))}
          </ul>
        </details>
      )}

      {failures.length > 0 && (
        <details className="warnbox error" open>
          <summary>{failures.length} 个文件下载失败（其余照常打包）</summary>
          <ul>
            {failures.slice(0, 50).map((f) => (
              <li key={f.plan.token}>
                <code>{f.plan.name}</code>：{f.error}
              </li>
            ))}
          </ul>
        </details>
      )}

      {records.length > 0 && (
        <section className="card">
          <h4>
            记录（{selected.size}/{records.length} 已选）
            <span className="inline-actions">
              <button onClick={() => toggleAll(true)} disabled={busy}>
                全选有附件的
              </button>
              <button onClick={() => toggleAll(false)} disabled={busy}>
                全不选
              </button>
            </span>
          </h4>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th />
                  <th>#</th>
                  <th>记录</th>
                  <th>部门</th>
                  <th>购买人</th>
                  <th>金额</th>
                  <th>日期</th>
                  <th>附件</th>
                </tr>
              </thead>
              <tbody>
                {records.map((r) => {
                  const count = SLOTS.reduce(
                    (n, s) => n + (r.attachments[s]?.length ?? 0),
                    0,
                  );
                  return (
                    <tr key={r.recordId} className={count === 0 ? 'empty' : ''}>
                      <td>
                        <input
                          type="checkbox"
                          checked={selected.has(r.recordId)}
                          disabled={busy || count === 0}
                          onChange={() => toggle(r.recordId)}
                        />
                      </td>
                      <td>{r.viewIndex}</td>
                      <td title={r.recordId}>{cellText(r.recordId).slice(0, 10)}…</td>
                      <td>{r.fields.department ?? '—'}</td>
                      <td>{r.fields.buyer ?? '—'}</td>
                      <td>{r.fields.amount ?? '—'}</td>
                      <td>{r.fields.date ?? '—'}</td>
                      <td>{SLOTS.map((s) => (r.attachments[s]?.length ? SLOT_LABEL[s] : '')).filter(Boolean).join(' ') || '无'}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {plan && plan.files.length > 0 && (
        <section className="card">
          <h4>文件预览（{plan.files.length}）</h4>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>zip 内路径</th>
                  <th>槽位</th>
                  <th>原始名</th>
                  <th>声明大小</th>
                </tr>
              </thead>
              <tbody>
                {plan.files.slice(0, 200).map((f) => (
                  <tr key={`${f.path}-${f.token}`}>
                    <td>{f.path}</td>
                    <td>{SLOT_LABEL[f.slot]}</td>
                    <td>{f.originalName}</td>
                    <td>{f.declaredSize ? `${Math.round(f.declaredSize / 1024)} KB` : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {plan.files.length > 200 && (
            <p className="hint">仅预览前 200 个；全部 {plan.files.length} 个都会被打包。</p>
          )}
          <p className="hint">已选 {selectedRows.length} 条记录。</p>
        </section>
      )}
    </div>
  );
}

function pad2(n: number): string {
  return String(n).padStart(2, '0');
}
