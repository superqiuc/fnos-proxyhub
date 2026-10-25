const fs=require('fs');
const code=fs.readFileSync(__dirname+'/g.js','utf8');
const fn=new Function('isPlainHostName','dnsDomainIs', code+'; return FindProxyForURL;');
const f=fn(h=>h.indexOf('.')<0,(h,d)=>h===d||h.endsWith(d));
for (const h of ['www.baidu.com','www.qq.com','www.google.com']) console.log(h,'->',f('https://'+h+'/',h));
