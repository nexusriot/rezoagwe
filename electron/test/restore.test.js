'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { NodeEngine } = require('../src/core/node-engine');
const { RestorePlans } = require('../src/core/restore');

test('restore preview selects keys and refuses concurrent additions',async()=>{
 const e=new NodeEngine({cluster:'test'},null);e.store.write('old','current');e.store.write('extra','keep');
 const plans=new RestorePlans(),p=plans.preview(e,{cluster:'test',entries:[{key:'old',value:'backup',action:'set'},{key:'new',value:'new',action:'set'}]},{deleteMissing:true});assert.equal(p.total,3);assert.equal(e.store.get('old'),'current');
 e.store.write('new','concurrent');const report=await plans.apply(e,p.id,['old','new']);assert.equal(report.restored,1);assert.equal(report.changed,1);assert.equal(e.store.get('new'),'concurrent');assert.equal(e.store.get('extra'),'keep');assert.throws(()=>plans.preview(e,{cluster:'other',entries:[]}));
});
