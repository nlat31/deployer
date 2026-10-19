import {useState} from 'react';
import {api,setCSRF} from './api';
type User={id:string,name:string,role:string,disabled:boolean,totp_enabled:boolean};
export function Login({onLogin}:{onLogin:(user:User)=>void}){
  const [challenge,setChallenge]=useState(''),[busy,setBusy]=useState(false),[error,setError]=useState('');
  return <div className="login-minimal"><form className="login-form" onSubmit={async e=>{e.preventDefault();if(busy)return;setBusy(true);setError('');const form=new FormData(e.currentTarget);try{const result=await api<{user:User,csrf:string,mfa_required?:boolean,challenge?:string}>(challenge?'/login/mfa':'/login','POST',challenge?{challenge,code:form.get('code')}:{username:form.get('username'),password:form.get('password')});if(result.mfa_required){setChallenge(result.challenge!)}else{setCSRF(result.csrf);onLogin(result.user)}}catch(e){setError((e as Error).message)}finally{setBusy(false)}}}>
    {challenge?<label>验证码或恢复码<input name="code" autoComplete="one-time-code" autoFocus required maxLength={100}/></label>:<><label>用户名<input name="username" autoComplete="username" defaultValue="admin" required maxLength={96}/></label><label>密码<input name="password" type="password" autoComplete="current-password" required maxLength={72}/></label></>}
    {error&&<p className="error-box" role="alert">{error}</p>}<button className="primary" disabled={busy}>{busy?'登录中…':'登录'}</button>{challenge&&<button className="login-back" type="button" disabled={busy} onClick={()=>{setChallenge('');setError('')}}>返回用户名和密码</button>}
  </form></div>
}
