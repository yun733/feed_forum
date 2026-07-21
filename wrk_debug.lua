-- wrk 调试脚本：打印响应状态码
local counter = {}
function response(status, headers, body)
    if not counter[status] then
        counter[status] = 0
    end
    counter[status] = counter[status] + 1
    -- 只打印前5个错误body
    if counter[status] <= 5 and status >= 400 then
        print("STATUS: " .. status .. " BODY: " .. body)
    end
end

function done(summary, latency, requests)
    print("---------- STATUS DISTRIBUTION ----------")
    for k, v in pairs(counter) do
        print(k .. ": " .. v)
    end
end
