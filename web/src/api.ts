let csrf = '';
export function setCSRF(value:string){csrf=value}
export async function api<T>(path:string,method='GET',body?:unknown):Promise<T>{
  const controller=new AbortController();const timer=setTimeout(()=>controller.abort(),body instanceof File?180000:30000);
  try{
    const res=await fetch('/api'+path,{method,signal:controller.signal,headers:{...(body instanceof File?{}:{'Content-Type':'application/json'}),...(method!=='GET'?{'X-CSRF-Token':csrf}:{})},body:body instanceof File?body:body!==undefined?JSON.stringify(body):undefined});
    const data=await res.json();if(!res.ok){if(res.status===401&&!path.startsWith('/login'))window.dispatchEvent(new Event('session-expired'));throw new Error(data.error||`请求失败 ${res.status}`)}return data as T;
  }catch(e){if(e instanceof Error&&e.name==='AbortError')throw new Error('请求超时，请刷新状态后重试');throw e}finally{clearTimeout(timer)}
}
