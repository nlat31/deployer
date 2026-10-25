import {test,expect} from '@playwright/test';
const digest='a'.repeat(64);
const release={version:'2.0',digest,os:'linux',arch:'amd64',size:12};
const node=(id:string,arch='amd64')=>({id,name:id,ip:id==='client-a'?'192.0.2.1':id==='client-b'?'192.0.2.2':'192.0.2.3',os:'linux',arch,last_seen:Date.now(),agent_update:{version:'1.0',digest:'old',phase:'committed',capable:true}});
async function fixture(page:any,admin=true){
 const state:any={releases:[],roll:{digest:'',nodes:[],index:0,paused:false},uploads:0,requests:[],deletes:[],actions:[],nodes:[node('client-a'),node('client-b'),node('arm','arm64')]};
 await page.route('**/api/**',async(route:any)=>{
 const req=route.request(),path=new URL(req.url()).pathname;let body:any={};
 if(path==='/api/session')body={csrf:'test-token',user:{id:'admin',name:'Admin',role:admin?'admin':'viewer'}};
 else if(path==='/api/snapshot')body={apps:state.apps??[{id:'demo',name:'Demo',health:{type:'process'}}],versions:[{app_id:'demo',version:'1',arch:'amd64',status:'available',size:10}],nodes:state.nodes,batches:state.batches??[],deployments:state.deployments??[{node_id:'client-a',app_id:'demo',current:'1',desired_version:'1',desired_state:'running',running_version:'1',run_state:'running',revision:1,health:state.healthError?'unhealthy':'healthy',error:state.healthError||'',cleanup_error:state.cleanupError||''}],operations:state.operations??[],inventories:state.inventories??{'client-a':[{app_id:'demo',version:'1',initialized:true,installed_at:1700000000000,started_at:1700000060000},{app_id:'demo',version:'2',initialized:true,installed_at:1700000120000,started_at:0}]}};
 else if(path==='/api/agent-releases'){
  if(req.method()==='POST'){expect(req.headers()['x-csrf-token']).toBe('test-token');expect(req.headers()['content-type']).toContain('multipart/form-data; boundary=');expect(req.postData()).toContain('name="manifest"');expect(req.postData()).toContain('name="binary"');state.uploads++;state.releases=[release];body=release}else body=state.releases;
 }else if(path==='/api/agent-rollout'){
  if(req.method()==='POST'){const data=req.postDataJSON();state.requests.push(data);state.roll=data.digest?{...data,index:0,paused:false}:{...state.roll,...data}}body=state.roll;
 }else if(path==='/api/batches'&&req.method()==='POST'){const data=req.postDataJSON();state.requests.push(data);state.batches=[{id:'batch-new',...data,index:0,skipped:0,failed:0,state:'running'}];body=state.batches[0]
 }else if(path.endsWith('/cancel')&&req.method()==='POST'){state.requests.push({path});const batch=state.batches?.find((b:any)=>path===`/api/batches/${b.id}/cancel`);if(batch)batch.state='cancelled';const op=state.operations?.find((o:any)=>path===`/api/operations/${o.id}/cancel`);if(op)op.cancel_requested=true;body={ok:true}
 }else if(path==='/api/nodes/offline'&&req.method()==='DELETE'){const old=state.nodes.length;state.nodes=state.nodes.filter((n:any)=>Date.now()-n.last_seen<30000);state.deletes.push(path);body={deleted:old-state.nodes.length}
 }else if(path.endsWith('/operations')&&req.method()==='POST'){const data=req.postDataJSON();state.actions.push({path,...data});body={id:'op-'+state.actions.length,app_id:'demo',node_id:'client-a',...data,result:'succeeded'}}else if(req.method()==='DELETE'){state.deletes.push(path);body={state:'pending'}};
 await route.fulfill({json:body});
 });
 await page.goto('/');return state;
}
test('upload, select compatible clients, rollout progress, pause, failure',async({page})=>{
 const s=await fixture(page);await page.getByRole('button',{name:'Agent 更新',exact:false}).click();
 await page.getByLabel('签名清单（manifest.json）').setInputFiles({name:'agent.manifest.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(release))});
 await page.getByLabel('Agent 程序（.bin）').setInputFiles({name:'agent.bin',mimeType:'application/octet-stream',buffer:Buffer.from('signed agent')});
 await page.getByRole('button',{name:'上传签名版本',exact:true}).click();await expect(page.getByText('签名与程序摘要校验通过，版本已上传')).toBeVisible();expect(s.uploads).toBe(1);
 await page.getByRole('checkbox',{name:'更新 client-a',exact:true}).check();await page.getByRole('checkbox',{name:'更新 client-b',exact:true}).check();await expect(page.getByRole('checkbox',{name:'更新 arm',exact:true})).toHaveCount(0);
 page.once('dialog',d=>d.accept());await page.getByRole('button',{name:'更新所选 2 台客户端'}).click();await expect.poll(()=>s.requests.length).toBe(1);expect(s.requests[0]).toEqual({digest,nodes:['client-a','client-b']});
 s.nodes[0].agent_update={version:'2.0',digest:'old',target:digest,phase:'trial',capable:true};await expect(page.getByRole('cell',{name:'试运行',exact:true})).toBeVisible();
 await page.getByRole('button',{name:'暂停批次',exact:true}).click();await expect.poll(()=>s.roll.paused).toBe(true);await expect(page.getByRole('button',{name:'继续批次',exact:true})).toBeVisible();
 s.roll.error='trial exited: exit status 42';s.nodes[0].agent_update={version:'1.0',digest:'old',target:digest,phase:'failed',error:'trial exited: exit status 42',capable:true};await expect(page.getByRole('cell',{name:'trial exited: exit status 42',exact:true})).toBeVisible();
 await page.screenshot({path:'test-results/agent-updates.png',fullPage:true});
});
test('parallel update progress records out-of-order completion',async({page})=>{
 const s=await fixture(page);s.releases=[release];s.roll={digest,nodes:['client-a','client-b'],index:0,paused:false,active:{'client-a':true},completed:{'client-b':true}};
 s.nodes[0].agent_update={version:'2.0',digest:'old',target:digest,phase:'trial',capable:true};
 s.nodes[1].agent_update={version:'2.0',digest,phase:'committed',capable:true};
 await page.getByRole('button',{name:'Agent 更新',exact:false}).click();
 await expect(page.getByText(/已完成 1\/2 台/)).toBeVisible();
 await expect(page.getByText(/最多 20 台并行更新/)).toBeVisible();
 const section=page.locator('section').filter({has:page.getByRole('heading',{name:'当前更新批次'})});
 await expect(section.getByRole('row').filter({hasText:'client-b'}).getByRole('cell',{name:'已完成',exact:true})).toBeVisible();
 await expect(section.getByRole('row').filter({hasText:'client-a'}).getByRole('cell',{name:'试运行',exact:true})).toBeVisible();
});
test('deletion requires confirmation describing stop and permanent data removal',async({page})=>{
 const s=await fixture(page);await page.getByRole('button',{name:'应用仓库',exact:false}).click();
 page.once('dialog',async d=>{expect(d.message()).toContain('先停止');expect(d.message()).toContain('数据');await d.dismiss()});await page.getByTitle('删除仓库包').click();expect(s.deletes).toHaveLength(0);
 page.once('dialog',d=>d.accept());await page.getByTitle('删除仓库包').click();await expect.poll(()=>s.deletes.length).toBe(1);expect(s.deletes[0]).toBe('/api/apps/demo/versions/1');
 page.once('dialog',async d=>{expect(d.message()).toContain('整个应用目录');await d.accept()});await page.getByRole('button',{name:'删除应用',exact:true}).click();await expect.poll(()=>s.deletes.length).toBe(2);
});
test('viewer cannot upload or create updates',async({page})=>{await fixture(page,false);await page.getByRole('button',{name:'Agent 更新',exact:false}).click();await expect(page.getByRole('button',{name:'上传签名版本'})).toHaveCount(0)});

test('application errors are visible and automatic rollback is unavailable',async({page})=>{
 const state=await fixture(page);state.healthError='component checksum mismatch';
 await page.getByRole('button',{name:'▤ 设备列表'}).click();await page.getByLabel('按应用筛选').selectOption('demo');
 await expect(page.getByText('component checksum mismatch',{exact:true})).toBeVisible();
 await page.getByRole('button',{name:'切换版本',exact:true}).click();
 await expect(page.getByRole('dialog',{name:'部署应用'})).toBeVisible();
 await expect(page.getByRole('checkbox',{name:'启动失败时尝试恢复旧版本'})).toHaveCount(0);
 await expect(page.getByText('版本之间的数据独立保存。部署或健康检查失败时仅报告错误，由管理员手动切换版本。')).toBeVisible();
});

 test('version inventory shows deployment and actual start times',async({page})=>{
 await fixture(page);await page.getByRole('button',{name:'▤ 设备列表'}).click();await page.getByLabel('按应用筛选').selectOption('demo');
 await expect(page.getByText('运行版本：1',{exact:true})).toBeVisible();
 await page.getByRole('button',{name:'版本管理',exact:true}).click();
 const dialog=page.getByRole('dialog',{name:'本地版本 · demo'});
 const first=dialog.locator('.local-version').filter({has:page.getByText('1',{exact:true})});
 const format=(n:number)=>new Date(n).toLocaleString('zh-CN',{hour12:false});
 await expect(first.getByText('运行中 · 已初始化',{exact:true})).toBeVisible();
 await expect(first.getByText('部署时间：'+format(1700000000000),{exact:true})).toBeVisible();
 await expect(first.getByText('最近启动：'+format(1700000060000),{exact:true})).toBeVisible();
 const second=dialog.locator('.local-version').filter({has:page.getByText('2',{exact:true})});
 await expect(second.getByText('最近启动：未记录',{exact:true})).toBeVisible();
 });

test('device filter is mandatory, undeployed devices expose only deploy, selection never crosses filters',async({page})=>{
 await fixture(page);await page.getByRole('button',{name:'▤ 设备列表'}).click();
 await expect(page.getByRole('button',{name:'应用部署',exact:true})).toHaveCount(0);
 await expect(page.locator('tbody tr')).toHaveCount(0);
 await page.getByLabel('按应用筛选').selectOption('__undeployed__');
 await expect(page.locator('tbody tr')).toHaveCount(2);
 for(const row of await page.locator('tbody tr').all()){await expect(row.getByRole('button')).toHaveText(['部署'])}
 await page.getByRole('button',{name:'全选在线设备'}).click();await expect(page.getByText('已选 2 / 共 2 台')).toBeVisible();
 await page.getByLabel('按应用筛选').selectOption('demo');
 await expect(page.getByText('已选 0 / 共 1 台')).toBeVisible();
 await expect(page.locator('tbody tr').getByRole('button')).toHaveText(['切换版本','停止','启动','日志','版本管理']);
 await expect(page.getByText('回退',{exact:false})).toHaveCount(0);
});
test('device actions address the selected app and explicit switch is not skipped',async({page})=>{
 const s=await fixture(page);await page.getByRole('button',{name:'▤ 设备列表'}).click();await page.getByLabel('按应用筛选').selectOption('demo');
 for(const [label,action] of [['停止','stop'],['启动','start'],['日志','logs']]){
  await page.getByRole('button',{name:label,exact:true}).click();await expect.poll(()=>s.actions.at(-1)?.action).toBe(action);
  expect(s.actions.at(-1).path).toBe('/api/nodes/client-a/apps/demo/operations');expect(s.actions.at(-1).expected_revision).toBe(1);
  await page.getByRole('dialog').getByLabel('关闭').click();
 }
 await page.getByRole('button',{name:'切换版本',exact:true}).click();
 await expect(page.getByRole('dialog').getByLabel('应用',{exact:true})).toBeDisabled();
 await page.getByLabel('目标版本').selectOption('1');await page.getByRole('button',{name:'创建部署任务'}).click();
 await expect.poll(()=>s.actions.at(-1)?.action).toBe('switch');expect(s.actions.at(-1).skip_if_deployed).toBe(false);
});
test('viewer cannot mutate device deployments',async({page})=>{
 await fixture(page,false);await page.getByRole('button',{name:'▤ 设备列表'}).click();await page.getByLabel('按应用筛选').selectOption('demo');
 for(const name of ['切换版本','停止','启动'])await expect(page.getByRole('button',{name,exact:true})).toBeDisabled();
 await page.getByRole('button',{name:'版本管理',exact:true}).click();
 for(const button of await page.getByRole('dialog').getByRole('button',{name:'删除版本',exact:true}).all())await expect(button).toBeDisabled();
});

test('heartbeat cleanup errors remain visible in the device list',async({page})=>{const s=await fixture(page);s.cleanupError='删除 demo/1 失败: stop script exited 42';await page.getByRole('button',{name:'▤ 设备列表'}).click();await page.getByLabel('按应用筛选').selectOption('demo');await expect(page.getByText(s.cleanupError,{exact:true})).toBeVisible()});

test('device selection is preserved across pages but cleared when changing app filter',async({page})=>{
 const s=await fixture(page);for(let i=0;i<10;i++)s.nodes.push({...node('extra-'+i),ip:'192.0.2.'+(10+i)});
 await page.getByRole('button',{name:'▤ 设备列表'}).click();await page.getByLabel('按应用筛选').selectOption('__undeployed__');
 await page.getByLabel('每页展示数量').selectOption('5');await page.getByLabel('选择本页在线设备').check();
 await expect(page.getByText('已选 5 / 共 12 台')).toBeVisible();await page.getByRole('button',{name:'下一页',exact:true}).click();await page.getByLabel('选择本页在线设备').check();
 await expect(page.getByText('已选 10 / 共 12 台')).toBeVisible();await page.getByRole('button',{name:'首页',exact:true}).click();await expect(page.getByLabel('选择本页在线设备')).toBeChecked();
 await page.getByLabel('按应用筛选').selectOption('demo');await expect(page.getByText('已选 0 / 共 1 台')).toBeVisible();
});

test('repository-missing app cannot receive a bulk version switch',async({page})=>{
 const s=await fixture(page);s.apps=[];
 await page.getByRole('button',{name:'▤ 设备列表'}).click();await expect(page.getByRole('option',{name:'demo（仓库已删除，等待清理） · demo'})).toHaveCount(1);
 await page.getByLabel('按应用筛选').selectOption('demo');await page.getByRole('button',{name:'全选在线设备'}).click();
 await expect(page.getByRole('button',{name:'切换所选设备版本',exact:true})).toBeDisabled();await expect(page.getByRole('button',{name:'切换版本',exact:true})).toBeDisabled();
});

test('delete all offline devices requires confirmation and preserves online devices',async({page})=>{
 const s=await fixture(page);s.nodes.push({...node('offline'),last_seen:Date.now()-60000});
 await page.getByRole('button',{name:'▤ 设备列表'}).click();
 const button=page.getByRole('button',{name:'删除离线设备',exact:true});await expect(button).toBeEnabled();
 page.once('dialog',d=>d.dismiss());await button.click();expect(s.deletes).toHaveLength(0);
 page.once('dialog',d=>d.accept());await button.click();await expect.poll(()=>s.deletes.length).toBe(1);
 expect(s.nodes).toHaveLength(3);await expect(button).toBeDisabled();
});
test('multiple selected devices can detach with permanent deletion confirmation',async({page})=>{
 const s=await fixture(page);s.inventories={'client-a':[{app_id:'demo',version:'1'}],'client-b':[{app_id:'demo',version:'1'}]};
 await page.getByRole('button',{name:'▤ 设备列表'}).click();await page.getByLabel('按应用筛选').selectOption('demo');
 await page.getByRole('button',{name:'全选在线设备'}).click();
 page.once('dialog',async d=>{expect(d.message()).toContain('永久删除');expect(d.message()).toContain('2 台');await d.accept()});
 await page.getByRole('button',{name:'批量解除部署',exact:true}).click();await expect.poll(()=>s.requests.length).toBe(1);
 expect(s.requests[0]).toMatchObject({app_id:'demo',nodes:['client-a','client-b'],request:{action:'detach',confirm:true}});
 await expect(page.getByText('demo · 批量解除部署')).toBeVisible();
});
test('deployment tasks and batches support cancellation and show per-device failures',async({page})=>{
 const s=await fixture(page);s.operations=[{id:'active',node_id:'client-a',app_id:'demo',action:'deploy',version:'1',stage:'setting_up',result:'',created_at:Date.now()}];
 s.batches=[{id:'batch',app_id:'demo',nodes:['client-a','client-b'],index:1,failed:1,skipped:0,state:'running',failures:{'client-b':'setup.sh failed: exit 42'}}];
 await page.getByRole('button',{name:'≡ 操作记录'}).click();await expect(page.getByText('192.0.2.2：setup.sh failed: exit 42')).toBeVisible();
 page.once('dialog',d=>d.accept());await page.getByRole('button',{name:'取消任务',exact:true}).click();await expect.poll(()=>s.operations[0].cancel_requested).toBe(true);
 page.once('dialog',d=>d.accept());await page.getByRole('button',{name:'取消批次',exact:true}).click();await expect.poll(()=>s.batches[0].state).toBe('cancelled');
 await expect(page.getByText('已取消',{exact:true})).toBeVisible();await page.screenshot({path:'test-results/task-controls.png',fullPage:true});
});
