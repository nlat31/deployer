import {test,expect} from '@playwright/test';
const digest='a'.repeat(64);
const release={version:'2.0',digest,os:'linux',arch:'amd64',size:12};
const node=(id:string,arch='amd64')=>({id,name:id,ip:'127.0.0.1',os:'linux',arch,last_seen:Date.now(),agent_update:{version:'1.0',digest:'old',phase:'committed',capable:true}});
async function fixture(page:any,admin=true){
 const state:any={releases:[],roll:{digest:'',nodes:[],index:0,paused:false},uploads:0,requests:[],deletes:[],nodes:[node('client-a'),node('client-b'),node('arm','arm64')]};
 await page.route('**/api/**',async(route:any)=>{
 const req=route.request(),path=new URL(req.url()).pathname;let body:any={};
 if(path==='/api/session')body={csrf:'test-token',user:{id:'admin',name:'Admin',role:admin?'admin':'viewer'}};
 else if(path==='/api/snapshot')body={apps:[{id:'demo',name:'Demo',health:{type:'process'}}],versions:[{app_id:'demo',version:'1',arch:'amd64',status:'available',size:10}],nodes:state.nodes,deployments:[{node_id:'client-a',app_id:'demo',current:'1',previous:'',desired_version:'1',desired_state:'running',run_state:'running',revision:1}],operations:[],inventories:{'client-a':[{app_id:'demo',version:'1',initialized:true}]}};
 else if(path==='/api/agent-releases'){
  if(req.method()==='POST'){expect(req.headers()['x-csrf-token']).toBe('test-token');expect(req.headers()['content-type']).toContain('multipart/form-data; boundary=');expect(req.postData()).toContain('name="manifest"');expect(req.postData()).toContain('name="binary"');state.uploads++;state.releases=[release];body=release}else body=state.releases;
 }else if(path==='/api/agent-rollout'){
  if(req.method()==='POST'){const data=req.postDataJSON();state.requests.push(data);state.roll=data.digest?{...data,index:0,paused:false}:{...state.roll,...data}}body=state.roll;
 }else if(req.method()==='DELETE'){state.deletes.push(path);body={state:'pending'}};
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
test('deletion requires confirmation describing stop and permanent data removal',async({page})=>{
 const s=await fixture(page);await page.getByRole('button',{name:'应用仓库',exact:false}).click();
 page.once('dialog',async d=>{expect(d.message()).toContain('先停止');expect(d.message()).toContain('数据');await d.dismiss()});await page.getByTitle('删除仓库包').click();expect(s.deletes).toHaveLength(0);
 page.once('dialog',d=>d.accept());await page.getByTitle('删除仓库包').click();await expect.poll(()=>s.deletes.length).toBe(1);expect(s.deletes[0]).toBe('/api/apps/demo/versions/1');
 page.once('dialog',async d=>{expect(d.message()).toContain('整个应用目录');await d.accept()});await page.getByRole('button',{name:'删除应用',exact:true}).click();await expect.poll(()=>s.deletes.length).toBe(2);
});
test('viewer cannot upload or create updates',async({page})=>{await fixture(page,false);await page.getByRole('button',{name:'Agent 更新',exact:false}).click();await expect(page.getByRole('button',{name:'上传签名版本'})).toHaveCount(0)});
