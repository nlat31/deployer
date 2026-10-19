import {defineConfig} from '@playwright/test';
export default defineConfig({testDir:'./tests',use:{baseURL:'http://127.0.0.1:17712',headless:true,channel:process.env.PLAYWRIGHT_CHANNEL||undefined},webServer:{command:'npm run dev -- --port 17712',url:'http://127.0.0.1:17712',reuseExistingServer:false},reporter:'list'});
