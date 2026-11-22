import { useEffect, useRef, useState, type FormEvent } from 'react'
import { AdminTokenError, readDevices, type Device } from '../../api/devices'
import { newID, projectNameBytes, saveProject, withinTextLimit, type Directory, type Project, type ProjectBinding, type ProjectInput, type Workspace } from '../../api/workspace'
import { messageFor } from '../../useWorkspace'

type ProjectsProps = {
  token: string
  workspace: Workspace | null
  onSaved: () => void
  onLock: (reason: string) => void
}

type ProjectDraft = { id: string; name: string; bindings: ProjectBinding[] }

const maxProjectBindings = 32

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

function projectInput(draft: ProjectDraft): ProjectInput {
  const primary = draft.bindings.find((binding) => binding.is_default) ?? draft.bindings[0]
  return {
    name: draft.name.trim(),
    agent_kind: primary?.agent_kind ?? 'dsh',
    bindings: draft.bindings,
    device_id: primary?.device_id ?? '',
    directory_id: primary?.directory_id ?? '',
  }
}

export function Projects({ token, workspace, onSaved, onLock }: ProjectsProps) {
  const [devices, setDevices] = useState<Device[]>([])
  const [draft, setDraft] = useState<ProjectDraft>()
  const [saveError, setSaveError] = useState('')
  const [saving, setSaving] = useState(false)
  const request = useRef<AbortController | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    void readDevices(token, controller.signal).then(setDevices).catch((failure: unknown) => {
      if (!controller.signal.aborted && failure instanceof AdminTokenError) onLock(failure.message)
    })
    return () => {
      controller.abort()
      request.current?.abort()
    }
  }, [token, onLock])

  function edit(project: ProjectDraft) {
    if (request.current) return
    setSaveError('')
    setDraft(project)
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!draft?.name.trim() || !withinTextLimit(draft.name, projectNameBytes) || !canSave || request.current) return
    const controller = new AbortController()
    request.current = controller
    setSaving(true)
    setSaveError('')
    try {
      await saveProject(token, draft.id, projectInput(draft), controller.signal)
      if (!controller.signal.aborted) {
        setDraft(undefined)
        onSaved()
      }
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminTokenError) onLock(failure.message)
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

  function create() {
    edit({ id: newID(), name: '', bindings: [newBinding(true)] })
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

  return (
    <section aria-labelledby="projects-heading">
      <div className="view-heading"><h1 id="projects-heading">项目</h1><button type="button" className="primary-button" onClick={create} disabled={saving || eligibleDeviceIDs.length === 0}>新建项目</button></div>
      {!workspace && <p className="panel-placeholder">正在读取项目…</p>}
      {workspace && eligibleDeviceIDs.length === 0 && <p className="panel-placeholder">先打开目标设备上的 Agent Muster，在设备端授权一个工作目录。</p>}
      {workspace && eligibleDeviceIDs.length > 0 && workspace.projects.length === 0 && !draft && <p className="panel-placeholder">暂无项目。新建项目并选用已授权的设备目录后，即可从主对话交办。</p>}
      {draft && <form className="project-form" onSubmit={submit}>
        <h2>{workspace?.projects.some((project) => project.id === draft.id) ? '编辑项目' : '新建项目'}</h2>
        <label className="project-name-field">项目名称<input value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} maxLength={projectNameBytes} disabled={saving} aria-invalid={!validName} aria-describedby={!validName ? 'project-name-error' : undefined} required /></label>
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
        {saveError && <p className="read-error" role="alert">{saveError}</p>}
        <div className="form-actions"><button className="quiet-button" type="button" onClick={() => setDraft(undefined)} disabled={saving}>取消</button><button className="primary-button" type="submit" disabled={saving || !draft.name.trim() || !validName || !canSave}>{saving ? '正在保存…' : '保存项目'}</button></div>
      </form>}
      {workspace && workspace.projects.length > 0 && <div className="project-list">
        {workspace.projects.map((project) => <article className="project-row project-bindings-row" key={project.id}>
          <div><h2>{project.name}</h2><div className="project-binding-summary">{bindingsFor(project).map((binding) => <p key={binding.id}><span>{binding.is_default ? '默认' : '可选'}</span> {deviceName(binding.device_id)} <span aria-hidden="true">·</span> {binding.directory_label} <span aria-hidden="true">·</span> {binding.agent_kind}</p>)}</div></div>
          <button className="outline-button" type="button" disabled={saving} onClick={() => edit({ id: project.id, name: project.name, bindings: bindingsFor(project) })}>编辑<span className="screen-reader"> {project.name}</span></button>
        </article>)}
      </div>}
    </section>
  )
}
