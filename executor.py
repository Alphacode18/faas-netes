import time, requests, sys

def get_monotonic_clock():
    return int(time.monotonic() * 1e9)

def get_replicas_number(json_response):
	#return int(json_response['availableReplicas'])
	return int(json_response.get('availableReplicas', 0))

def ensure_scale_from_zero(protocol, gateway_address, function_name, reconciliation_time, max_tries):
	count = 0
	while True:
		req_url = protocol + gateway_address + '/function/' + function_name
		info_req = requests.get(req_url)
		
		if(int(info_req.status_code) < 300):
			n_replicas = get_replicas_number(info_req.json())
			
			if(n_replicas == 0):
				print ("Zero replicas")
				break
			elif(count > max_tries):
				message = 'Too much tries, please ensure the zero scale funcionality in OpenFaaS [gateway configuration, faas-idler configuration and function deployment configuration]'
				raise RuntimeError(message)
			else:
				count += 1
		else:
			message = 'Request to [' + req_url + '] status response [' + str(info_req.status_code) + ']'
			raise RuntimeError('Request error: ' + message)
		
		time.sleep(reconciliation_time)

def function_request(protocol, gateway_address, function_name):
	req_url = protocol + gateway_address + '/function/' + function_name
	start_ts = get_monotonic_clock()
	response = requests.post(req_url, data={})
	end_ts = get_monotonic_clock()

	if(int(response.status_code) < 300):
		return (response.headers, response.content, end_ts - start_ts)
	else:
		message = 'Request to [' + req_url + '] status response [' + str(response.status_code) + ']'
		raise RuntimeError('Request error: ' + message)

def get_ordered_metrics(headers, response):
	duration_time = int(float(headers['X-Duration-Seconds']) * (10**9))
	#app_startup_time = int(headers['X-App-Startup-Time', 0])
	app_startup_time = int(headers.get('X-App-Startup-Time', 0))
	return [app_startup_time, duration_time]

def main():
	args = sys.argv

	reconciliation_time = 10 # 10s
	max_tries = 15 # 2min30s

	protocol = 'http://'
	gateway_address = args[1]
	function_name = args[2]
	total_requests = int(args[3])

	csv_file = open(function_name + '-' + str(total_requests) + '-' + str(int(time.time())) + '.csv', 'w')
	csv_file.write('Req_Id,AppStartup_Time,Duration_Time,Latency_Time\n')

	for req_id in range(total_requests):
		print (req_id)
		try:
			ensure_scale_from_zero(protocol, gateway_address, function_name, reconciliation_time, max_tries)
			(headers, response, latency) = function_request(protocol, gateway_address, function_name)
			metrics = get_ordered_metrics(headers, response)
			ts_str = str(req_id) + ',' + ",".join([str(i) for i in metrics]) + ',' + str(latency) + '\n'
			csv_file.write(ts_str)
			print (ts_str)
		except Exception as e:
			print (repr(e))
			continue

	csv_file.close()

main()
