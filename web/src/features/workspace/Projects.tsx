import { useEffect, useRef, useState, type FormEvent } from 'react'
import { readDevices, type Device } from '../../api/devices'
import { AdminSessionError } from '../../api/request'
import { newID, projectNameBytes, saveProject, withinTextLimit, type Directory, type GitPolicy, type Project, type ProjectBinding, type ProjectInput, type Workspace } from '../../api/workspace'
import { messageFor } from '../../useWorkspace'
import './projects.css'

type ProjectsProps = {
  workspace: Workspace | null
  onSaved: () => void
  onLock: (reason: string) => void
}

type ProjectDraft = { id: string; name: string; bindings: ProjectBinding[]; gitPolicy: GitPolicyDraft; initialGitPolicy: string }

/** 编辑时保留开关和短分支名，独立参数使用稳定键以便逐项编辑。 */
type GitPolicyDraft = {
  enabled: boolean
  baseRef: string
  remote: string
  targetBranch: string
  candidateCommit: NonNullable<GitPolicy['candidate_commit']>
  program: string
  args: { id: string; value: string }[]
  timeoutSeconds: string
  reviewAgent: GitPolicy['review_agent'] | ''
  maxRepairs: string
}

const maxProjectBindings = 32
const branchPrefix = 'refs/heads/'

/** 将已保存的规则转为表单值，旧项目默认关闭。 */
function gitPolicyDraft(policy?: GitPolicy): GitPolicyDraft {
  return {
    enabled: Boolean(policy),
    baseRef: policy?.base_ref ?? 'refs/remotes/origin/main',
    remote: policy?.remote ?? 'origin',
    targetBranch: policy?.target_branch?.replace(/^refs\/heads\//, '') ?? '',
    candidateCommit: policy?.candidate_commit ?? '',
    program: policy?.test.program ?? '',
    args: (policy?.test.args ?? []).map((value) => ({ id: newID(), value })),
    timeoutSeconds: String(policy?.test.timeout_seconds ?? 120),
    reviewAgent: policy?.review_agent ?? '',
    maxRepairs: String(policy?.max_repairs ?? 0),
  }
}

function bindingsFor(project: Project): ProjectBinding[] {
  if (project.bindings?.length) return project.bindings
  if (!project.directory_id) return []
  return [{
    id: project.id,
    device_id: project.device_id,
    directory_id: project.directory_id,
    directory_label: project.directory_label,
    agent_kind: project.agent_kind || 'dsh',
    is_default: true,
  }]
}

/** 生成用于提交和比较的规则，关闭交付时生成 null。 */
function gitPolicyInput(policy: GitPolicyDraft): Omit<GitPolicy, 'revision'> | null {
  if (!policy.enabled) return null
  const targetBranch = policy.targetBranch.trim()
  return {
    base_ref: policy.baseRef.trim(),
    remote: policy.remote.trim(),
    target_branch: targetBranch ? `${branchPrefix}${targetBranch}` : undefined,
    candidate_commit: policy.candidateCommit || undefined,
    test: { program: policy.program.trim(), args: policy.args.map(({ value }) => value), timeout_seconds: Number(policy.timeoutSeconds) },
    review_agent: policy.reviewAgent || undefined,
    max_repairs: Number(policy.maxRepairs),
  }
}

/** 只提交发生变化的规则，避免项目编辑覆盖其他会话保存的规则。 */
function projectInput(draft: ProjectDraft): ProjectInput {
  const primary = draft.bindings.find((binding) => binding.is_default) ?? draft.bindings[0]
  const policy = gitPolicyInput(draft.gitPolicy)
  return {
    name: draft.name.trim(),
    agent_kind: primary?.agent_kind ?? 'dsh',
    bindings: draft.bindings,
    device_id: primary?.device_id ?? '',
    directory_id: primary?.directory_id ?? '',
    git_policy: JSON.stringify(policy) === draft.initialGitPolicy ? undefined : policy,
  }
}

/** 编辑项目级 Git 交付规则，参数按独立输入保存。 */
function GitDeliveryFields({ policy, saving, onChange }: { policy: GitPolicyDraft; saving: boolean; onChange: (policy: GitPolicyDraft) => void }) {
  return <fieldset className="project-git-policy" disabled={saving}>
    <legend>Git 交付</legend>
    <label className="default-binding"><input type="checkbox" checked={policy.enabled} onChange={(event) => onChange({ ...policy, enabled: event.target.checked })} />启用 Git 交付</label>
    {policy.enabled && <div className="git-policy-fields">
      <label>基准引用<input value={policy.baseRef} onChange={(event) => onChange({ ...policy, baseRef: event.target.value })} placeholder="refs/remotes/origin/main" required /></label>
      <label>远程仓库别名<input value={policy.remote} onChange={(event) => onChange({ ...policy, remote: event.target.value })} placeholder="origin" required /></label>
      <label>目标分支<input value={policy.targetBranch} onChange={(event) => onChange({ ...policy, targetBranch: event.target.value })} placeholder="留空时按任务生成" /></label>
      <label>候选提交方式<select value={policy.candidateCommit} onChange={(event) => {
        onChange({ ...policy, candidateCommit: event.target.value === 'daemon' ? 'daemon' : '' })
      }}>
        <option value="">Agent 提交</option>
        <option value="daemon">设备端提交</option>
      </select></label>
      <label>测试程序<input value={policy.program} onChange={(event) => onChange({ ...policy, program: event.target.value })} placeholder="go" required /></label>
      <label>测试超时（秒）<input type="number" min={1} max={3600} step={1} value={policy.timeoutSeconds} onChange={(event) => onChange({ ...policy, timeoutSeconds: event.target.value })} required /></label>
      <label>Review Agent<select value={policy.reviewAgent} onChange={(event) => {
        const value = event.target.value
        onChange({ ...policy, reviewAgent: value === 'dsh' || value === 'codex' ? value : '' })
      }}>
        <option value="">关闭 Review</option>
        <option value="dsh">dsh</option>
        <option value="codex">Codex</option>
      </select></label>
      <label>最多返修次数<input type="number" min={0} max={5} step={1} value={policy.maxRepairs} onChange={(event) => onChange({ ...policy, maxRepairs: event.target.value })} required /></label>
      <div className="git-policy-args" role="group" aria-label="测试参数">
        <button className="outline-button" type="button" onClick={() => onChange({ ...policy, args: [...policy.args, { id: newID(), value: '' }] })}>添加测试参数</button>
        {policy.args.map((arg, index) => <div className="git-policy-arg" key={arg.id}>
          <textarea rows={1} aria-label={`测试参数 ${index + 1}`} value={arg.value} onChange={(event) => {
            const value = event.target.value
            onChange({ ...policy, args: policy.args.map((item) => item.id === arg.id ? { ...item, value } : item) })
          }} />
          <button type="button" className="quiet-button" onClick={() => onChange({ ...policy, args: policy.args.filter((item) => item.id !== arg.id) })}>移除<span className="screen-reader">测试参数 {index + 1}</span></button>
        </div>)}
      </div>
    </div>}
  </fieldset>
}

export function Projects({ workspace, onSaved, onLock }: ProjectsProps) {
  const [devices, setDevices] = useState<Device[]>([])
  const [draft, setDraft] = useState<ProjectDraft>()
  const [saveError, setSaveError] = useState('')
  const [saving, setSaving] = useState(false)
  const request = useRef<AbortController | null>(null)
  const nameInput = useRef<HTMLInputElement>(null)
  const editorTrigger = useRef<HTMLButtonElement>(null)
  const draftID = draft?.id

  useEffect(() => {
    const controller = new AbortController()
    void readDevices(controller.signal).then(setDevices).catch((failure: unknown) => {
      if (!controller.signal.aborted && failure instanceof AdminSessionError) onLock(failure.message)
    })
    return () => {
      controller.abort()
      request.current?.abort()
    }
  }, [onLock])

  useEffect(() => {
    if (draftID) nameInput.current?.focus()
    else editorTrigger.current?.focus()
  }, [draftID])

  function edit(project: Omit<ProjectDraft, 'initialGitPolicy'>, trigger: HTMLButtonElement) {
    if (request.current) return
    editorTrigger.current = trigger
    setSaveError('')
    setDraft({ ...project, initialGitPolicy: JSON.stringify(gitPolicyInput(project.gitPolicy)) })
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!draft?.name.trim() || !withinTextLimit(draft.name, projectNameBytes) || !canSave || request.current) return
    const controller = new AbortController()
    request.current = controller
    setSaving(true)
    setSaveError('')
    try {
      await saveProject(draft.id, projectInput(draft), controller.signal)
      if (!controller.signal.aborted) {
        setDraft(undefined)
        onSaved()
      }
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminSessionError) onLock(failure.message)
      else setSaveError(messageFor(failure))
    } finally {
      request.current = null
      if (!controller.signal.aborted) setSaving(false)
    }
  }

  const directories = workspace?.directories ?? []
  const deviceByID = new Map(devices.map((device) => [device.id, device]))
  const eligibleDeviceIDs = [...new Set(directories.map((directory) => directory.device_id))].filter((id) => !deviceByID.get(id)?.revoked_at)
  const deviceName = (id: string) => deviceByID.get(id)?.name ?? id
  const deviceDirectories = (deviceID: string): Directory[] => directories.filter((directory) => directory.device_id === deviceID)
  const validName = withinTextLimit(draft?.name ?? '', projectNameBytes)
  const canSave = !!draft && draft.bindings.every((binding) =>
    eligibleDeviceIDs.includes(binding.device_id) && deviceDirectories(binding.device_id).some((directory) => directory.id === binding.directory_id) &&
    (binding.agent_kind === 'dsh' || binding.agent_kind === 'codex'))

  function newBinding(isDefault: boolean): ProjectBinding {
    const device_id = eligibleDeviceIDs.length === 1 ? eligibleDeviceIDs[0] : ''
    const localDirectories = deviceDirectories(device_id)
    const directory_id = localDirectories.length === 1 ? localDirectories[0].id : ''
    const candidate = deviceByID.get(device_id)?.agents.find((agent) => agent.status === 'supported')
    return { id: newID(), device_id, directory_id, directory_label: localDirectories.find((directory) => directory.id === directory_id)?.label ?? '', agent_kind: candidate?.id ?? '', is_default: isDefault }
  }

  function create(trigger: HTMLButtonElement) {
    edit({ id: newID(), name: '', bindings: [newBinding(true)], gitPolicy: gitPolicyDraft() }, trigger)
  }

  function updateBinding(index: number, patch: Partial<ProjectBinding>) {
    if (!draft) return
    const bindings = draft.bindings.map((binding, position) => {
      if (position !== index) return patch.is_default ? { ...binding, is_default: false } : binding
      return { ...binding, ...patch }
    })
    setDraft({ ...draft, bindings })
  }

  function addBinding() {
    if (!draft || draft.bindings.length >= maxProjectBindings) return
    setDraft({ ...draft, bindings: [...draft.bindings, newBinding(false)] })
  }

  function removeBinding(index: number) {
    if (!draft || draft.bindings.length <= 1) return
    const bindings = draft.bindings.filter((_, position) => position !== index)
    if (!bindings.some((binding) => binding.is_default)) bindings[0] = { ...bindings[0], is_default: true }
    setDraft({ ...draft, bindings })
  }

  const existingProject = workspace?.projects.some(({ id }) => id === draftID)
  const editor = draft && (
    <form className="project-form" onSubmit={submit}>
        {!existingProject && <h2>新建项目</h2>}
        <label className="project-name-field">项目名称<input ref={nameInput} value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} maxLength={projectNameBytes} disabled={saving} aria-invalid={!validName} aria-describedby={!validName ? 'project-name-error' : undefined} required /></label>
        {!validName && <p id="project-name-error" className="read-error" role="status">项目名称最多 {projectNameBytes} 个 UTF-8 字节，且不能包含空字符。</p>}
        <div className="binding-heading"><h3>可用执行绑定</h3><button className="outline-button" type="button" disabled={saving || eligibleDeviceIDs.length === 0 || draft.bindings.length >= maxProjectBindings} onClick={addBinding}>添加绑定</button></div>
        <div className="binding-list">{draft.bindings.map((binding, index) => {
          const localDirectories = deviceDirectories(binding.device_id)
          const agents = deviceByID.get(binding.device_id)?.agents ?? []
          return <article className="binding-row" key={binding.id}>
            <div className="binding-fields">
              <label>设备<select value={binding.device_id} onChange={(event) => {
                const device_id = event.target.value
                const nextDirectories = deviceDirectories(device_id)
                const directory_id = nextDirectories.length === 1 ? nextDirectories[0].id : ''
                const candidate = deviceByID.get(device_id)?.agents.find((agent) => agent.status === 'supported')
                updateBinding(index, { device_id, directory_id, directory_label: nextDirectories.find((directory) => directory.id === directory_id)?.label ?? '', agent_kind: candidate?.id ?? '' })
              }} disabled={saving} required>
                <option value="">选择设备</option>
                {binding.device_id && !eligibleDeviceIDs.includes(binding.device_id) && <option value={binding.device_id}>{deviceName(binding.device_id)}（不可用）</option>}
                {eligibleDeviceIDs.map((id) => <option key={id} value={id}>{deviceName(id)}</option>)}
              </select></label>
              <label>工作目录<select value={binding.directory_id} onChange={(event) => updateBinding(index, { directory_id: event.target.value, directory_label: localDirectories.find((directory) => directory.id === event.target.value)?.label ?? '' })} disabled={saving || !binding.device_id} required>
                <option value="">选择已授权目录</option>
                {localDirectories.map((directory) => <option key={directory.id} value={directory.id}>{directory.label}</option>)}
              </select></label>
              <label>Agent<select value={binding.agent_kind} onChange={(event) => updateBinding(index, { agent_kind: event.target.value })} disabled={saving || !binding.device_id} required>
                <option value="">选择 Agent</option>
                {(['dsh', 'codex'] as const).map((kind) => {
                  const agent = agents.find(({ id }) => id === kind)
                  const probeLabel = agent?.status === 'supported' ? '' : '（能力未确认）'
                  return <option key={kind} value={kind}>{kind === 'dsh' ? 'dsh' : 'Codex'}{probeLabel}</option>
                })}
              </select></label>
            </div>
            <div className="binding-actions"><label className="default-binding"><input type="radio" name={`default-binding-${draft.id}`} checked={binding.is_default} onChange={() => updateBinding(index, { is_default: true })} disabled={saving} />默认绑定</label><button type="button" className="quiet-button" onClick={() => removeBinding(index)} disabled={saving || draft.bindings.length <= 1}>移除<span className="screen-reader">执行绑定</span></button></div>
          </article>
        })}</div>
        <GitDeliveryFields policy={draft.gitPolicy} saving={saving} onChange={(gitPolicy) => setDraft({ ...draft, gitPolicy })} />
        {saveError && <p className="read-error" role="alert">{saveError}</p>}
        <div className="form-actions"><button className="quiet-button" type="button" onClick={() => setDraft(undefined)} disabled={saving}>取消</button><button className="primary-button" type="submit" disabled={saving || !draft.name.trim() || !validName || !canSave}>{saving ? '正在保存…' : '保存项目'}</button></div>
      </form>
  )

  function bindingSummary(binding: ProjectBinding) {
    return <div className="project-binding">
      <span>{deviceName(binding.device_id)} · {binding.agent_kind === 'codex' ? 'Codex' : binding.agent_kind}</span>
      <span>{binding.directory_label}</span>
    </div>
  }

  return (
    <section aria-labelledby="projects-heading">
      <div className="view-heading">
        <h1 id="projects-heading">项目</h1>
        <button type="button" className="primary-button" onClick={(event) => create(event.currentTarget)} disabled={saving || eligibleDeviceIDs.length === 0}>新建项目</button>
      </div>
      {!workspace && <p className="panel-placeholder">正在读取项目…</p>}
      {workspace && eligibleDeviceIDs.length === 0 && <p className="panel-placeholder">请先在设备端授权工作目录。</p>}
      {workspace && eligibleDeviceIDs.length > 0 && workspace.projects.length === 0 && !draft && <p className="panel-placeholder">暂无项目。</p>}
      {!existingProject && editor}
      {workspace && workspace.projects.length > 0 && <div className="projects-list">
        {workspace.projects.map((project) => {
          const bindings = bindingsFor(project)
          const primary = bindings.find(({ is_default }) => is_default)
          const alternatives = bindings.filter((binding) => binding !== primary)
          return <article className="project-entry" key={project.id}>
            <header className="project-entry-heading">
              <h2>{project.name}</h2>
              <button className="quiet-button" type="button" disabled={saving || draftID === project.id} onClick={(event) => edit({ id: project.id, name: project.name, bindings, gitPolicy: gitPolicyDraft(project.git_policy) }, event.currentTarget)}>编辑<span className="screen-reader"> {project.name}</span></button>
            </header>
            {draftID === project.id ? editor : <>
              {primary && <div className="project-primary-binding">
                <span>默认绑定</span>
                {bindingSummary(primary)}
              </div>}
              {alternatives.length > 0 && <details className="project-alternatives">
                <summary>其他绑定（{alternatives.length}）</summary>
                <ul>{alternatives.map((binding) => <li key={binding.id}>{bindingSummary(binding)}</li>)}</ul>
              </details>}
              {!bindings.length && <p className="project-unbound">暂无绑定</p>}
            </>}
          </article>
        })}
      </div>}
    </section>
  )
}
