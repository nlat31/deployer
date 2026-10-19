import React, {useEffect, useRef, useState} from 'react';

type Host={id:string,name:string,ip:string,arch:string,last_seen:number,free_bytes:number,certificate_expires:number};
type Deployment={node_id:string,app_id:string,current:string,run_state:string,health:string};
type Props={nodes:Host[],deployments:Deployment[],canEdit:boolean,admin:boolean,busy:boolean,onDeploy:(ids:string[])=>void,onDelete:(node:Host)=>void};
const online=(n:Host)=>Date.now()-n.last_seen<30000;
const date=(n:number)=>n?new Date(n).toLocaleString('zh-CN',{hour12:false}):'—';
const space=(n:number)=>(n/1073741824).toFixed(1)+' GB';
export function Hosts({nodes,deployments,canEdit,admin,busy,onDeploy,onDelete}:Props){
 const [page,setPage]=useState(1),[pageSize,setPageSize]=useState(20),[jump,setJump]=useState('1'),[selected,setSelected]=useState<Set<string>>(new Set());
 const header=useRef<HTMLInputElement>(null);
 const pages=Math.max(1,Math.ceil(nodes.length/pageSize)),current=Math.min(page,pages);
 const ordered=[...nodes].sort((a,b)=>(a.ip||'\uffff').localeCompare(b.ip||'\uffff',undefined,{numeric:true})||a.id.localeCompare(b.id));
 const visible=ordered.slice((current-1)*pageSize,current*pageSize),eligible=ordered.filter(online),visibleEligible=visible.filter(online);
 const chosen=nodes.filter(n=>selected.has(n.id));
 const allVisible=visibleEligible.length>0&&visibleEligible.every(n=>selected.has(n.id));
 useEffect(()=>{setPage(p=>Math.min(p,pages))},[pages]);
 useEffect(()=>{setJump(String(current))},[current]);
 useEffect(()=>{const ids=new Set(nodes.map(n=>n.id));setSelected(prev=>{const next=new Set([...prev].filter(id=>ids.has(id)));return next.size===prev.size?prev:next})},[nodes]);
 useEffect(()=>{if(header.current)header.current.indeterminate=!allVisible&&visibleEligible.some(n=>selected.has(n.id))},[selected,visibleEligible,allVisible]);
 function toggle(id:string){setSelected(prev=>{const next=new Set(prev);next.has(id)?next.delete(id):next.add(id);return next})}
 function selectPage(){setSelected(prev=>{const next=new Set(prev);for(const n of visibleEligible){allVisible?next.delete(n.id):next.add(n.id)}return next})}
 const unavailable=chosen.some(n=>!online(n));
 return <section className="panel host-list">
  <div className="host-toolbar"><div className="actions"><button disabled={!canEdit||!eligible.length} onClick={()=>setSelected(new Set(eligible.map(n=>n.id)))}>全选在线主机</button><button disabled={!canEdit||!eligible.length} onClick={()=>setSelected(new Set(eligible.filter(n=>!selected.has(n.id)).map(n=>n.id)))}>反选在线主机</button><button disabled={!selected.size} onClick={()=>setSelected(new Set())}>清空选择</button><span>已选 {chosen.length} / 共 {nodes.length} 台</span></div><button className="primary" disabled={!canEdit||busy||!chosen.length||unavailable||chosen.length>100} onClick={()=>onDeploy(chosen.map(n=>n.id))}>部署所选主机</button></div>
  {unavailable&&<p className="host-hint">选中的主机已离线，请取消勾选后部署。</p>}{chosen.length>100&&<p className="host-hint">每批最多部署 100 台主机，请减少选择。</p>}
  <div className="table-wrap"><table className="hosts-table"><thead><tr><th><input ref={header} type="checkbox" aria-label="选择本页在线主机" checked={allVisible} disabled={!canEdit||!visibleEligible.length} onChange={selectPage}/></th><th>IP 地址 / 主机名称</th><th>状态</th><th>主机 ID</th><th>平台</th><th>剩余空间</th><th>已部署应用</th><th>最后心跳</th><th>证书到期</th><th>操作</th></tr></thead><tbody>{visible.map(n=>{const apps=deployments.filter(d=>d.node_id===n.id&&d.current);return <tr key={n.id} className={selected.has(n.id)?'selected-row':''}><td><input type="checkbox" aria-label={`选择主机 ${n.ip||n.name||n.id}`} checked={selected.has(n.id)} disabled={!canEdit||!online(n)&&!selected.has(n.id)} onChange={()=>toggle(n.id)}/></td><td><strong>{n.ip||'IP 未记录'}</strong><small>{n.name}</small></td><td><span className={'badge '+(online(n)?'good':'muted')}><i/>{online(n)?'在线':'离线'}</span></td><td><code className="host-identity" title={n.id}>{n.id}</code></td><td>linux/{n.arch}</td><td>{space(n.free_bytes)}</td><td>{apps.length?apps.map(d=><small key={d.app_id}><strong>{d.app_id} {d.current}</strong> · {online(n)?(d.run_state==='running'?'运行中':d.run_state==='stopped'?'已停止':d.run_state):'离线'}</small>):<small>未部署</small>}</td><td className="mono">{date(n.last_seen)}</td><td className="mono">{date(n.certificate_expires)}</td><td><div className="actions"><button disabled={!canEdit||busy||!online(n)} onClick={()=>onDeploy([n.id])}>部署</button>{admin&&!online(n)&&<button className="danger" disabled={busy} onClick={()=>onDelete(n)}>删除主机</button>}</div></td></tr>})}</tbody></table></div>
  {!nodes.length&&<div className="empty"><h3>等待第一台主机接入</h3><p>启动 Agent 后，合法 mTLS 心跳会自动登记。</p></div>}
  <div className="host-pagination"><label>每页<select aria-label="每页展示数量" value={pageSize} onChange={e=>{setPageSize(Number(e.target.value));setPage(1)}}>{[5,10,20,50,100].map(n=><option key={n} value={n}>{n} 条</option>)}</select></label><span>共 {nodes.length} 条 · 第 {current} / {pages} 页</span><button disabled={current===1} onClick={()=>setPage(1)}>首页</button><button disabled={current===1} onClick={()=>setPage(current-1)}>上一页</button><button disabled={current===pages} onClick={()=>setPage(current+1)}>下一页</button><button disabled={current===pages} onClick={()=>setPage(pages)}>末页</button><form onSubmit={e=>{e.preventDefault();const n=Number(jump);if(Number.isInteger(n)&&n>=1&&n<=pages)setPage(n)}}><label>跳至<input aria-label="跳转页码" type="number" min={1} max={pages} value={jump} onChange={e=>setJump(e.target.value)} required/></label><button type="submit">跳转</button></form></div>
 </section>
}
